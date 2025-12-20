module vehicle-service

go 1.22

require (
	github.com/librescoot/librefsm v0.0.0-00010101000000-000000000000
	github.com/redis/go-redis/v9 v9.14.1
	github.com/warthog618/go-gpiocdev v0.9.1
	golang.org/x/sys v0.28.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
)

replace github.com/librescoot/librefsm => ../librefsm
