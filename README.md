# elegooctl

A LAN CLI for Elegoo printers using SDCP WebSocket status, ElegooLink file
uploads and the printer's MJPEG camera. Tested against the Centauri Carbon
with firmware 1.1.40; other models/firmwares may differ.

## Build

```sh
go build ./cmd/elegooctl
go test ./...
go vet ./...
```

Go 1.27.2 or newer is required. Only `record` needs an additional executable,
`ffmpeg`. The Nix package includes it on PATH:

```sh
nix build
./result/bin/elegooctl --help
```

The Nix package and development shell explicitly use Go 1.27 from the pinned
Nixpkgs input; CI uses the version in `go.mod`.

## Commands

Supply the printer hostname explicitly; the binary has no configured device,
account, bot token, chat ID or personal paths. Connection flags precede the
command. Default ports are SDCP 3030, file HTTP 80 and camera HTTP 3031.

```sh
elegooctl --host printer.example status
elegooctl --host printer.example attributes
elegooctl --host printer.example query --command 320 --data '{}'

elegooctl --host printer.example upload part.gcode
elegooctl --host printer.example upload --wait-idle --timeout 2h part.gcode
elegooctl --host printer.example upload --verify-only part.gcode

mkdir -p captures recordings
elegooctl --host printer.example snapshot --output captures/finished.jpg
elegooctl --host printer.example watch --file part.gcode --output captures/ready.json
elegooctl --host printer.example record --file part.gcode --output recordings/part
elegooctl --host printer.example jog --axis Z --step 5
```

`captures/` and `recordings/` are git-ignored. Choose new output filenames and a
new recording subdirectory for each run; existing outputs are not overwritten.

- **Upload:** streams in 1 MiB chunks and downloads the published file to verify
  its size and MD5. Handles delayed file publication. With `--wait-idle`, waits
  and retries if the printer rejects an upload. Never starts a print. Do not
  modify the local file while an upload is in progress.
- **Snapshot:** reads one JPEG directly from the camera, without motion or
  homing. Refuses an existing output file.
- **Watch:** attaches to the current print, or waits for the exact `--file`.
  Locks onto its task ID. A later print produces a `canceled` event and exits
  successfully. Readiness requires idle, bed heat off and three readings at or
  below the threshold (default 30 °C, configurable 20–35 °C). It distinguishes
  completion from stopping before the expected layer count.
- **Record:** same job attachment rules. Saves timestamped status JSONL and MP4
  segments, reconnecting after camera failures. The output directory must be new.
  `recording.json` records segments, camera failures and termination errors;
  interruptions preserve partial recordings. Stops if another job replaces the
  selected task. A final JPEG is captured when the printer becomes idle.
- **Jog:** the only command that requests motion. Sends one explicit relative
  move, at most 100 mm, after checking idle status and coordinates. Uses
  conservative target bounds 5–250 mm for the supported 256 mm printer. It does
  not home axes or automatically recover from a rejected movement command.
- **Query:** permits only the known read-only command numbers
  258, 260, 320, 321 and 323. Payload details are firmware-specific.

No command starts prints, changes temperatures or homes the printer. JSON data
goes to stdout; progress and errors go to stderr. SIGINT/SIGTERM cancel requests
and finalize recordings. `watch`/`record` default to a 24-hour overall timeout;
recording itself defaults to a six-hour limit after job attachment.

## Network and output privacy

Use this tool on a trusted LAN. Printer commands use plain WebSocket (`ws://`),
and file transfers and camera capture use plain HTTP. The CLI supplies no
authentication or encryption. MD5 verification checks transfer integrity; it
does not authenticate the printer or the file's origin.

Status, attributes, query results and recording traces can contain device
identifiers, network addresses, print filenames, task IDs and timestamps.
Snapshots and videos show the printer camera's field of view. Review and redact
these outputs before sharing them, including in issue reports. Keep runtime
output in the ignored directories above or outside the checkout.

## Notification integration

Telegram and desktop notification delivery belong outside the printer CLI.
Use a separate executable via `--notify-command`:

```sh
mkdir -p captures
elegooctl --host printer.example --notify-command /path/to/print-notifier \
  watch --file part.gcode --output captures/ready.json
```

On readiness it receives this JSON on stdin, without shell evaluation:

```json
{
  "event": "removal-ready",
  "filename": "part.gcode",
  "task_id": "example-task",
  "completed": true,
  "bed_temperature": 29.8,
  "time": "2026-01-01T12:00:00Z"
}
```

The notification is attempted once; nonzero exit status fails the watch.
Canceled jobs do not invoke the notification command. `--output` refuses an
existing marker. Delivery and marker creation are not a transactional
exactly-once mechanism: inspect output before retrying a failed notification.

## Configuration wrappers

Keep personal device settings in your own configuration repository:

```sh
#!/bin/sh
exec elegooctl --host printer.example --notify-command /path/to/print-notifier "$@"
```

Power-management integration is external too. For long-running jobs on a Linux
desktop you can use `systemd-inhibit ... elegooctl ... record/watch`. The core
binary does not change host sleep policy or install background services.

## Protocol references

- [CBD's SDCP V3.0.0 specification](https://github.com/cbd-tech/SDCP-Smart-Device-Control-Protocol-V3.0.0/blob/main/SDCP%28Smart%20Device%20Control%20Protocol%29_V3.0.0_EN.md)
  describes the base WebSocket protocol and chunked HTTP upload format.
- [CentauriPilot's Centauri Carbon protocol notes](https://github.com/hataketsu/CentauriPilot/blob/091e31b26297250778aa60c4baf62fc3b951b05f/docs/SDCP_PROTOCOL.md)
  document firmware-specific commands and MJPEG camera behavior. Those notes
  cover firmware 1.1.46; elegooctl's tested firmware is 1.1.40.

| Operation | Command or endpoint | Reference |
| --- | --- | --- |
| Status and attributes | `0`, `1` | SDCP V3.0.0 |
| File list, history IDs, task details | `258`, `320`, `321` | SDCP V3.0.0 |
| File details, history video | `260`, `323` | Centauri firmware extensions |
| Relative axis move | `401` with `Axis` and `Step` | Centauri firmware extension |
| File upload | `/uploadFile/upload` | SDCP upload format; Centauri uses HTTP port 80 |
| Camera | HTTP port 3031, `/video` | Centauri MJPEG stream |

The CLI uses the read-only query allowlist and explicit jog command described
above. Payloads, ports and camera transports can differ on other models or
firmwares; the base SDCP specification alone does not establish compatibility.

## Development

```sh
nix develop
go test -race -count=1 ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
nix flake check
```

CI builds the CLI and runs the race-enabled tests, vet and vulnerability scan
on Linux. The vulnerability scan uses the current Go vulnerability database.

Tests use local mock HTTP/WebSocket servers and a simulated ffmpeg process;
they do not connect to a physical printer or messaging service. Coverage includes
chunk boundaries, checksum rejection, camera parsing, no-overwrite behavior,
recording reconnection, job identity, early stops and cooling conditions.

Runtime recordings, status dumps, G-code and device configuration are not part
of this source repository. Use the ignored output directories or keep runtime
output outside the checkout.

## License

[MIT](LICENSE).
