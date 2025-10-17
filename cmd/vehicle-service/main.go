package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"vehicle-service/internal/core"
)

func main() {
	// Disable timestamps when running under systemd (which adds its own)
	if os.Getenv("INVOCATION_ID") != "" {
		log.SetFlags(0)
	} else {
		log.SetFlags(log.LstdFlags)
	}
	log.Printf("Starting vehicle service...")

	system := core.NewVehicleSystem("127.0.0.1", 6379)
	if err := system.Start(); err != nil {
		log.Fatalf("Failed to start system: %v", err)
	}

	log.Printf("System started successfully")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)
	system.Shutdown()
	log.Printf("Shutdown complete")
}
