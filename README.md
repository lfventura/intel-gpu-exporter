# intel-gpu-exporter

Prometheus exporter for Intel GPUs on the `i915` and `xe` drivers (Arc A-series, Arc B-series /
Battlemage, Arc Pro and integrated GPUs). Every card in the host is monitored at once, with
per-process engine use and memory (including VRAM).

It reads only files the kernel already keeps, the same way for both drivers, and does not use
`intel_gpu_top` or the GPU PMU. That matters for `xe` cards: the `xe` PMU only exists from kernel
6.15, so on older kernels (TrueNAS SCALE 25.x runs 6.12) `intel_gpu_top` fails with
"Failed to detect engines".

Based on [mike1808/igpu-exporter](https://github.com/mike1808/igpu-exporter), whose metric names it
keeps so existing dashboards keep working.

## Sources

| Data | Source |
|---|---|
| Engine busy, per process and per card | `/proc/<pid>/fdinfo/<fd>` of DRM nodes: `drm-cycles-*` / `drm-total-cycles-*` (xe), `drm-engine-*` ns (i915), `drm-engine-capacity-*` |
| Memory per process and per card | fdinfo `drm-{total,shared,resident,purgeable,active}-<region>` |
| VRAM size | `lmem_total_bytes` when the driver has it, else the largest PCI BAR (whole VRAM with resizable BAR) |
| Frequency | xe: `device/tile0/gt0/freq0/{act,cur}_freq`; i915: `gt_{act,cur}_freq_mhz` |
| RC6 (idle) | xe: `device/tile*/gt*/gtidle/idle_residency_ms`; i915: `gt/gt*/rc6_residency_ms` |
| Power | hwmon `energy*_input` (the sensor labelled `card`, else the first one that counts) |
| Interrupts | `/proc/interrupts` lines naming the card's PCI address (or its driver, when it is the only card on it) |

The `/proc` scan follows only fds that link to `/dev/dri/*`. Every sample re-reads the processes
with a GPU node open and checks the processes started since the previous sample, so a new GPU
process (an ffmpeg transcode) shows up on the next sample. Every process is scanned again every
`FULL_SCAN_INTERVAL_S`, for processes that open a GPU node long after they start. On a TrueNAS host
with two cards this costs about 0.3% of one core at the default settings.

## Metrics

Every series has a `card` label with the PCI address (`0000:03:00.0`), which, unlike `cardN`, does
not change between boots.

| Metric | Labels | Unit |
|---|---|---|
| `igpu_info` | `card`, `driver`, `node` | always 1 |
| `igpu_engines_busy_percent` | `card`, `engine` | % of the engine class (0–100) |
| `igpu_frequency_actual`, `igpu_frequency_requested` | `card` | MHz |
| `igpu_rc6` | `card` | % of time idle, averaged over the GTs |
| `igpu_power_gpu`, `igpu_power_package` | `card` | W (needs hwmon energy counters: discrete cards) |
| `igpu_interrupts` | `card` | per second |
| `igpu_period` | `card` | ms between samples |
| `igpu_memory_total_bytes` | `card`, `region="vram"` | bytes |
| `igpu_memory_used_bytes` | `card`, `region` (`vram`, `system`) | bytes resident, summed over processes |
| `igpu_client_memory_{total,shared,resident,purgeable,active}_bytes` | `card`, `pid`, `name`, `region` | bytes |
| `igpu_client_engine_busy_percent` | `card`, `pid`, `name`, `engine` | % of one engine |

`engine` is one of `Render/3D`, `Blitter`, `Compute`, `Video`, `VideoEnhance`. Per process the value
is not divided by the number of engines in the class, so a process using both video engines of a
card shows up to 200%; the per-card value is normalised to 0–100%. `region="system"` is host RAM
mapped by the GPU (`system`, `gtt`); `vram` is the card's memory (`vram0` on xe, `local0` on i915).

Shared buffers count once in every process that maps them, so `igpu_memory_used_bytes` can be a
little higher than what the card really holds.

Not available without the PMU: engine `sema`/`wait` and IMC bandwidth. Temperature is not exported:
`i915` and `xe` hwmon do not expose it on 6.12 kernels.

## Configuration

| Variable | Default | |
|---|---|---|
| `PORT` | `8080` | HTTP port of `/metrics` |
| `REFRESH_PERIOD_MS` | `10000` | sample period |
| `FULL_SCAN_INTERVAL_S` | `60` | how often every process, not only new ones, is scanned for GPU clients |
| `CARDS` | all | comma-separated PCI addresses to monitor |
| `PROC_ROOT`, `SYSFS_ROOT` | `/proc`, `/sys` | for tests |
| `DEBUG` | off | log the collection time of every sample |

## Running

The container must see the host's processes (`pid: host`) and read other users' `/proc/<pid>/fd`,
so it runs privileged as root. It does not need `/dev/dri`.

```yaml
services:
  intel-gpu-exporter:
    image: ghcr.io/lfventura/intel-gpu-exporter:1.0.1
    container_name: intel-gpu-exporter
    restart: unless-stopped
    pid: host
    privileged: true
    ports:
      - "7071:8080"
    environment:
      REFRESH_PERIOD_MS: "10000"
```

Without `pid: host` the exporter only sees its own container and reports no per-process data.

## Building

```bash
go test ./cmd/
docker build -t intel-gpu-exporter .
```

Images are published to `ghcr.io/lfventura/intel-gpu-exporter` on every push to `main` (`main`,
`latest`, `main-<sha>`) and on `v*` tags (`1.0.0`, `1.0`).
