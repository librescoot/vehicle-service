module vehicle-service

go 1.24.0

require (
	github.com/librescoot/librefsm v0.4.0
	github.com/librescoot/redis-ipc v0.10.3
	github.com/warthog618/go-gpiocdev v0.9.1
	golang.org/x/sys v0.41.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/redis/go-redis/v9 v9.18.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
)

replace github.com/librescoot/librefsm => ../librefsm
