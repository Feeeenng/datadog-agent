name "protobuf-py-cpp"
default_version "3.4.0"

source :url => "https://github.com/google/protobuf/releases/download/v#{version}/protobuf-python-#{version}.tar.gz",
       :sha256 => "0bc10bfd00a9614fae58c86c21fbcf339790e48accf6d45f098034de985f5405"

relative_path "protobuf-#{version}/python"

env = {}

if ohai["platform_family"] == "mac_os_x"
  env["MACOSX_DEPLOYMENT_TARGET"] = "10.9"
end

build do
  ship_license "https://raw.githubusercontent.com/google/protobuf/3.1.x/LICENSE"

  if ohai["platform_family"] == "debian"
    # C++ runtime
    command ["cd .. && ./configure",
                "--prefix=#{install_dir}/embedded",
                "--enable-static=no",
                "--without-zlib"].join(" ")

    # You might want to temporarily uncomment the following line to check build sanity (e.g. when upgrading the
    # library) but there's no need to perform the check every time.
    # command "cd .. && make check"
    command "cd .. && make -j #{workers}"
    command "cd .. && make install"
  end
end
