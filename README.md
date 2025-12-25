# LibreScoot Vehicle Service

[![CC BY-NC-SA 4.0][cc-by-nc-sa-shield]][cc-by-nc-sa]

The LibreScoot Vehicle Service is a core component of the LibreScoot platform, responsible for managing and controlling electric scooter hardware systems. This service handles real-time vehicle operations, safety features, and communication with the dashboard system.

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

This work is licensed under a
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png
[cc-by-nc-sa-shield]: https://img.shields.io/badge/License-CC%20BY--NC--SA%204.0-lightgrey.svg

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

---

Made with ❤️ by the LibreScoot community
