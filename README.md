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

The service requires Redis connection details:
- Redis host
- Redis port

Additional configuration options can be found in the hardware constants and system configuration files.

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
