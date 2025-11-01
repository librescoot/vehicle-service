package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"vehicle-service/internal/core"
	"vehicle-service/internal/logger"
)

func main() {
	// Service log level
	var serviceLogLevel int
	flag.IntVar(&serviceLogLevel, "log", 3, "Service log level (0=NONE, 1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")

	flag.Parse()

	// Create standard logger with appropriate format
	var stdLogger *log.Logger
	if os.Getenv("INVOCATION_ID") != "" {
		// Running under systemd, use minimal format
		stdLogger = log.New(os.Stdout, "", 0)
	} else {
		// Running interactively, use timestamps
		stdLogger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds|log.Lmsgprefix)
	}

	// Create leveled logger
	l := logger.NewLogger(stdLogger, logger.LogLevel(serviceLogLevel))

	l.Infof("Starting vehicle service...")

	system := core.NewVehicleSystem("127.0.0.1", 6379, l)
	if err := system.Start(); err != nil {
		l.Fatalf("Failed to start system: %v", err)
	}

	l.Infof("System started successfully")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	l.Infof("Received signal %v, shutting down...", sig)
	system.Shutdown()
	l.Infof("Shutdown complete")
}
