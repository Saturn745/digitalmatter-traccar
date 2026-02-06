{
  lib,
  buildGo125Module,
}:
buildGo125Module {
  pname = "digitalmatter-traccar";
  version = "v0.0.1";
  src = ./.;
  subPackages = ["."];
  vendorHash = null;

  env.CGO_ENABLED = 0;
  ldflags = ["-extldflags=-static"];

  meta = with lib; {
    mainProgram = "digitalmatter-traccar";
    platforms = platforms.linux;
  };
}
