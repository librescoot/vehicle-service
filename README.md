# Librescoot Vehicle Service

The Librescoot Vehicle Service is a core component of the Librescoot platform, responsible for managing and controlling electric scooter hardware systems. This service handles real-time vehicle operations, safety features, and communication with the dashboard system.

Part of the [Librescoot](https://librescoot.org/) open-source platform.

## Features

- Real-time vehicle state management
- Hardware I/O control (GPIO)
- LED control system
- Handlebar locking mechanism
- Blinker control system
- Seat box locking mechanism
- Horn control
- Redis-based messaging system for component communication
- Safety state transitions
- Dashboard communication interface
- Fault reporting to `vehicle:fault` and `events:faults`

## Dependencies

- `github.com/redis/go-redis/v9` - Redis client for Go
- `github.com/warthog618/go-gpiocdev` - GPIO device interface
- `golang.org/x/sys` - System calls and primitives

## System Architecture

The service is built around a core `VehicleSystem` that manages:
- System state transitions
- Hardware I/O operations
- Real-time communication with the dashboard
- Safety features and interlocks
- User input processing

### Key Components

- **Core System**: Manages the overall vehicle state and coordinates between components
- **Hardware IO**: Interfaces with physical GPIO pins for input/output operations
- **Messaging**: Handles Redis-based communication between vehicle components
- **LED Control**: Manages vehicle lighting systems
- **State Management**: Ensures safe state transitions and vehicle operation

## Fault Reporting

Faults are reported under the `vehicle` group: the code goes into the
`vehicle:fault` set, an entry with the description lands in `events:faults`, and
a notification is published on the `vehicle` channel. Raising and clearing are
idempotent and run as one server-side script, so the set and the stream cannot
drift. A cleared fault appears in the stream with a negative code.

The codes are a contract once released. Numbers are never reused, and the list
is append only. Codes with no raise site yet are reserved so a later change
cannot claim them.

| Code | Meaning | Raised |
|---|---|---|
| 1 | Steering lock retries exhausted, sensor still reads unlocked | reserved |
| 2 | Steering unlock failed | reserved |
| 3 | `engine_power` output write failed | yes |
| 4 | `dashboard_power` output write failed | yes |
| 5 | `engine_brake` output write failed | yes |
| 6 | `seatbox_lock` output write failed | reserved |
| 10 | Input event device unreadable | reserved |
| 20 | Saved state could not be restored, vehicle forced to stand-by | yes |

Codes 3, 4 and 5 clear on the next successful write to the same channel, which
happens on every state transition, and for `engine_brake` on every brake lever
edge. Code 20 stands for the rest of the power session and is cleared by the
startup reconcile on the next boot, because the vehicle is not in the state it
was left in until then.

Every owned code is cleared once at startup, right after the GPIO lines have
been re-requested: a process that just started has no evidence of a standing
failure, and anything genuinely broken re-raises within one state transition.

The description reaches the stream and is rendered verbatim by the dashboard at
critical severity, with no per-code lookup table. It names the specific failure
and carries the underlying error. The first raise of a code wins the
description, since raising an already-active code writes nothing.

## Boot State Restore

`vehicle[state]` is read back at startup and the FSM is put into it. Only known,
resumable states are accepted; an unrecognised string or `updating` (which has
no transition in or out) is refused, the vehicle is left in stand-by with the
engine brake engaged and the ECU dark, the steering lock is armed if the sensor
reads unlocked, and fault 20 is raised.

`vehicle:restore-attempt` records the state a restore is entering and is deleted
once the restore has run to a conclusion. Finding it on the next boot means the
previous attempt never finished, most likely because the process died inside an
entry action, so that state is refused rather than tried again. The key carries
a 60s TTL and is process bookkeeping, not a published surface.

A failed entry action is never fatal. State machine entry actions are side
effects: the state is already committed when they run, and only a guard can stop
a transition.

## Building and Running

To build the service:

```bash
make build
```

To run the service:

```bash
./vehicle-service
```

## Configuration

### Command Line Options

- `--version`: Print version and exit
- `--log`: Service log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG, default: 3)

### Redis Configuration

The service connects to Redis at `127.0.0.1:6379` (hardcoded).

### LED Channel Mapping

The vehicle service controls 8 PWM LED channels with the following mappings:

| Index | LED Name              | Description                    |
|-------|-----------------------|--------------------------------|
| 0     | Headlight            | Main front illumination       |
| 1     | Front ring           | Front accent lighting          |
| 2     | Brake light          | Rear brake indicator           |
| 3     | Blinker front left   | Left front turn signal        |
| 4     | Blinker front right  | Right front turn signal       |
| 5     | Number plates        | License plate illumination    |
| 6     | Blinker rear left    | Left rear turn signal         |
| 7     | Blinker rear right   | Right rear turn signal        |

Channels 3, 4, 6, and 7 are configured as blinker channels and do not use adaptive mode.

#### LED Channel Modes

The PWM LED system supports two operational modes for each channel:

- **Adaptive Mode**: When enabled, causes the channel to adapt fade playback by finding the first duty-cycle value in the fade that is nearest to the current duty-cycle, then starting the fade from that point. This prevents abrupt jumps in brightness when transitioning between different LED states. Non-blinker channels (0, 1, 2, 5) use adaptive mode for smooth transitions.

- **Active/Inactive Mode**: Controls whether fade values are actually output to the LED. When active, fade values are set as the channel's duty-cycle normally. When inactive, the output is forced to 0% regardless of the fade being played. Blinker channels (3, 4, 6, 7) rely on precise active/inactive control for their flashing patterns.

For more detailed information about these modes, see the [i.MX PWM LED kernel module documentation](https://github.com/unumotors/kernel-module-imx-pwm-led/blob/master/README.md).

## Safety Features

The service implements several safety features:
- Handlebar position monitoring
- State-based operation restrictions
- Key card authentication
- Safe state transitions
- Emergency shutdown capabilities

## License

This project is dual-licensed. The source code is available under the
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].
The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

---

Made with ❤️ by the Librescoot community
