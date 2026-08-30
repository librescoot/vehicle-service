# Librescoot Vehicle Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

`vehicle-service` is the vehicle-control process for Librescoot. It combines a
vehicle state machine with Linux GPIO and LED control, and exposes vehicle
state and commands through Redis.

## Capabilities

- Manages vehicle state transitions and associated hardware actions.
- Controls vehicle GPIO for power, braking, locks, horn, seatbox, and inputs.
- Drives the PWM LED channels used for lighting and indicators.
- Processes handlebar, brake, seatbox, kickstand, and button input events.
- Publishes vehicle state, input events, and faults for other vehicle services.

## Operation and Redis interface

Redis is fixed at `127.0.0.1:6379`. The service publishes the `vehicle` hash,
including the vehicle state and hardware/input fields such as brake, blinker,
seatbox, kickstand, handlebar, power, and update state. It publishes UI input
edges on `buttons` and synthesized gestures on `input-events`.

Commands are consumed from these Redis lists:

- `scooter:seatbox`, `scooter:horn`, `scooter:blinker`, and `scooter:state`
- `scooter:led:cue` and `scooter:led:fade`
- `scooter:update`, `scooter:dbc-hold`, `scooter:hardware`, and `scooter:hop-on`

The service also watches `dashboard`, `keycard`, `settings`, `ota`,
`power-manager`, and `ble` hashes. It uses the hash-and-notification convention:
a channel message identifies the changed field, while the current value remains
in the corresponding hash.

Active faults are recorded in `vehicle:fault`, changes are published on the
`vehicle` channel as `fault`, and fault events are appended to `events:faults`.

## Configuration

Configuration is intentionally limited. The only command-line controls are
logging and version output; run `bin/vehicle-service-host -help` after
`make build-host` for the generated help. Redis connection settings and GPIO
mapping are compiled into the service.

Several `settings` hash fields alter supported vehicle behavior at runtime,
including the `scooter.*` settings for braking, standby, locking, horn,
indicator LEDs, and USB policy. Treat writes to `settings` and the command
queues as privileged: they can cause physical vehicle actions.

## Build and test

```bash
make build        # Linux ARMv7 binary: bin/vehicle-service
make build-host   # local-development binary: bin/vehicle-service-host
make test
make lint         # requires golangci-lint
```

`make fmt`, `make deps`, and `make clean` are also available.

## Deployment and operations

The Yocto layer ships `librescoot-vehicle.service`, which requires Valkey.
The target system must provide the datastore on the loopback address and grant
the process access to the GPIO, input, and LED devices used by the vehicle
hardware. A development binary does not exercise that hardware without an
appropriate target environment.

The process handles `SIGINT` and `SIGTERM` and runs its shutdown path before
exiting. Vehicle state and fault data are operational interfaces; consumers
should not infer a safe physical condition from Redis alone.

## License

This project is licensed under the [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License](LICENSE).

Made with ❤️ by the Librescoot community
