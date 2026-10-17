{
  lib,
  buildGoModule,
  makeWrapper,
  ffmpeg-headless,
}:
buildGoModule {
  pname = "elegooctl";
  version = "0.1.0";
  src = lib.cleanSource ./.;
  vendorHash = "sha256-7JqwcenKhaOgziXfkl32qz2VrZ0zKhNJMLfvHto/Pco=";
  env.CGO_ENABLED = 0;
  subPackages = [ "cmd/elegooctl" ];
  checkPhase = ''
    runHook preCheck
    go test ./...
    runHook postCheck
  '';
  ldflags = [
    "-s"
    "-w"
  ];
  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    wrapProgram "$out/bin/elegooctl" --prefix PATH : ${lib.makeBinPath [ ffmpeg-headless ]}
  '';
  meta = {
    description = "LAN status, verified uploads, camera capture and job monitoring for Elegoo printers";
    mainProgram = "elegooctl";
    platforms = lib.platforms.linux;
  };
}
