// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

// +build !windows

package container

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/datadog-agent/pkg/tagger"
	log "github.com/cihub/seelog"

	"github.com/DataDog/datadog-agent/pkg/logs/auditor"
	"github.com/DataDog/datadog-agent/pkg/logs/config"
	"github.com/DataDog/datadog-agent/pkg/logs/message"
	"github.com/DataDog/datadog-agent/pkg/logs/pipeline"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

const scanPeriod = 10 * time.Second
const dockerAPIVersion = "1.25"

// A Scanner listens for stdout and stderr of containers
type Scanner struct {
	pp         pipeline.Provider
	sources    []*config.LogSource
	tailers    map[string]*DockerTailer
	cli        *client.Client
	auditor    *auditor.Auditor
	ticker     *time.Ticker
	mu         *sync.Mutex
	shouldStop bool
}

// New returns an initialized Scanner
func New(sources []*config.LogSource, pp pipeline.Provider, a *auditor.Auditor) *Scanner {

	containerSources := []*config.LogSource{}
	for _, source := range sources {
		switch source.Config.Type {
		case config.DockerType:
			containerSources = append(containerSources, source)
		default:
		}
	}

	return &Scanner{
		pp:      pp,
		sources: containerSources,
		tailers: make(map[string]*DockerTailer),
		auditor: a,
		ticker:  time.NewTicker(scanPeriod),
		mu:      &sync.Mutex{},
	}
}

// Start starts the Scanner
func (s *Scanner) Start() {
	err := s.setup()
	if err == nil {
		go s.run()
	}
}

// Stop stops the Scanner and its tailers
func (s *Scanner) Stop() {
	s.mu.Lock()
	s.ticker.Stop()
	s.shouldStop = true
	wg := &sync.WaitGroup{}
	for _, tailer := range s.tailers {
		// stop all tailers in parallel
		wg.Add(1)
		go func(t *DockerTailer) {
			t.Stop(true)
			wg.Done()
		}(tailer)
		delete(s.tailers, tailer.ContainerID)
	}
	wg.Wait()
	s.mu.Unlock()
}

// run lets the Scanner tail docker stdouts
func (s *Scanner) run() {
	for range s.ticker.C {
		s.scan(true)
	}
}

// scan checks for new containers we're expected to
// tail, as well as stopped containers or containers that
// restarted
func (s *Scanner) scan(tailFromBeginning bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shouldStop {
		// no docker container should be tailed if stopped
		return
	}

	runningContainers := s.listContainers()
	containersToMonitor := make(map[string]bool)

	// monitor new containers, and restart tailers if needed
	for _, container := range runningContainers {
		for _, source := range s.sources {
			if s.sourceShouldMonitorContainer(source, container) {
				containersToMonitor[container.ID] = true

				tailer, isTailed := s.tailers[container.ID]
				if isTailed && tailer.shouldStop {
					s.stopTailer(tailer)
					isTailed = false
				}
				if !isTailed {
					s.setupTailer(s.cli, container, source, tailFromBeginning, s.pp.NextPipelineChan())
				}
			}
		}
	}

	// stop old containers
	for containerID, tailer := range s.tailers {
		_, shouldMonitor := containersToMonitor[containerID]
		if !shouldMonitor {
			s.stopTailer(tailer)
		}
	}
}

func (s *Scanner) stopTailer(tailer *DockerTailer) {
	tailer.Stop(false)
	delete(s.tailers, tailer.ContainerID)
}

func (s *Scanner) listContainers() []types.Container {
	containers, err := s.cli.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		log.Error("Can't tail containers, ", err)
		log.Error("Is datadog-agent part of docker user group?")
		return []types.Container{}
	}
	return containers
}

// sourceShouldMonitorContainer returns whether a container matches a log source configuration.
// Both image and label may be used:
// - If the source defines an image, the container must match it exactly.
// - If the source defines one or several labels, at least one of them must match the labels of the container.
func (s *Scanner) sourceShouldMonitorContainer(source *config.LogSource, container types.Container) bool {
	if source.Config.Image != "" && container.Image != source.Config.Image {
		return false
	}
	if source.Config.Label != "" {
		// Expect a comma-separated list of labels, eg: foo:bar, baz
		for _, value := range strings.Split(source.Config.Label, ",") {
			// Trim whitespace, then check whether the label format is either key:value or key=value
			label := strings.TrimSpace(value)
			parts := strings.FieldsFunc(label, func(c rune) bool {
				return c == ':' || c == '='
			})
			// If we have exactly two parts, check there is a container label that matches both.
			// Otherwise fall back to checking the whole label exists as a key.
			if _, exists := container.Labels[label]; exists || len(parts) == 2 && container.Labels[parts[0]] == parts[1] {
				return true
			}
		}
		return false
	}
	return true
}

// Start starts the Scanner
func (s *Scanner) setup() error {
	if len(s.sources) == 0 {
		return fmt.Errorf("No container source defined")
	}

	// List available containers

	cli, err := client.NewEnvClient()
	// Docker's api updates quickly and is pretty unstable, best pinpoint it
	cli.UpdateClientVersion(dockerAPIVersion)
	s.cli = cli
	if err != nil {
		log.Error("Can't tail containers,", err)
		return fmt.Errorf("Can't initialize client")
	}

	// Initialize docker utils
	err = tagger.Init()
	if err != nil {
		log.Warn(err)
	}

	// Start tailing monitored containers
	s.scan(false)
	return nil
}

// setupTailer sets one tailer, making it tail from the beginning or the end
func (s *Scanner) setupTailer(cli *client.Client, container types.Container, source *config.LogSource, tailFromBeginning bool, outputChan chan message.Message) {
	log.Info("Detected container ", container.Image, " - ", s.humanReadableContainerID(container.ID))
	t := NewDockerTailer(cli, container, source, outputChan)
	var err error
	if tailFromBeginning {
		err = t.tailFromBeginning()
	} else {
		err = t.recoverTailing(s.auditor)
	}
	if err != nil {
		log.Warn(err)
	}
	s.tailers[container.ID] = t
}

func (s *Scanner) humanReadableContainerID(containerID string) string {
	return containerID[:12]
}
