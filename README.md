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

`scooter:state lock-hibernate` submits one manual request to PM. PM admits explicit sleep from `parked` or `stand-by`; from `parked` it sends internal `prepare-hibernate:<request-id>`. Vehicle-service validates that ID against `power-manager[hibernate-request-id]` inside the parked-state FSM transition, then performs graceful dashboard and vehicle shutdown without submitting a second power request. Obsolete preparation cannot initiate locking. Rejection reports `hibernate-preparation-failed:<request-id>` on `scooter:power`.

Confirmed physical brake hibernation submits its manual power request only after publishing `stand-by`. Unlocking during hibernation preparation cancels the pending power intent. Dashboard installation inhibitors belong to their installer and are not removed by hibernation.

The service also watches `dashboard`, `keycard`, `settings`, `ota`,
`power-manager`, and `ble` hashes. It uses the hash-and-notification convention:
a channel message identifies the changed field, while the current value remains
in the corresponding hash.

Active faults are recorded in `vehicle:fault`, changes are published on the
`vehicle` channel as `fault`, and fault events are appended to `events:faults`.

## Starting a ride

Raising the kickstand requests driving. The vehicle stays `parked`, with the
motor interlock engaged, while battery-service completes normal activation.
Entry to `ready-to-drive` requires a present, active pack, the hardware 48V
signal, a closed seatbox, and fresh ECU evidence of a stopped wheel and released
throttle. Normal entry also requires dashboard readiness and an unlocked
handlebar latch. The three-button override retains the supply and throttle gate.

Battery/ECU hash reads and `engine-ecu:motion` samples expire after 500 ms.
The motion sample must follow 1.5 seconds of uninterrupted main and engine
power. Cached speed or throttle fields cannot authorize a start. ECU-service
requests a full status snapshot while parked and waiting for ECU evidence,
throttle release, or wheel standstill. Unchanged values are valid evidence
when reported in a fresh motion frame.

A pending start retries automatically. After 30 seconds it remains blocked until
a new kickstand cycle or a vehicle lock/unlock cycle. Lowering the kickstand,
or locking cancels the pending request. Only parked retries automatically.
Supply loss during a ride does not trigger automatic battery reactivation. Restoring a saved driving
state without an active supply returns to parked; an already powered ride can
retain its state across a vehicle-service restart.

`vehicle[drive-start-status]` uses the normal `vehicle` field notification:

| Value | Meaning |
| --- | --- |
| empty | No pending start warning |
| `waiting-battery` | No present, active battery confirmed |
| `waiting-power` | Main or engine power unavailable |
| `waiting-ecu` | Healthy, fresh ECU evidence not yet available |
| `release-throttle` | ECU reported throttle applied; a fresh released sample is required |
| `stop-vehicle` | ECU reported wheel movement; a fresh stopped sample is required |
| `close-seatbox` | Seatbox is open or its sensor cannot be read |
| `timeout` | Start timed out or its evidence changed before motor release; retry required |

Battery voltage matching and admission protections remain owned by
battery-service. No second-pack voltage limit is relaxed by this gate.

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
