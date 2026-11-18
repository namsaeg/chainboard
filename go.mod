module github.com/namsaeg/chainboard

go 1.24.0

replace golang.org/x/crypto => github.com/golang/crypto v0.41.0

replace golang.org/x/sys => github.com/golang/sys v0.35.0

replace golang.org/x/exp => github.com/golang/exp v0.0.0-20250819193227-8b4c13bb791b

replace golang.org/x/sync => github.com/golang/sync v0.16.0

replace rsc.io/tmplfunc => github.com/rsc/tmplfunc v0.0.3

replace go.etcd.io/bbolt => github.com/etcd-io/bbolt v1.4.3

require (
	github.com/consensys/gnark v0.14.0
	github.com/consensys/gnark-crypto v0.19.0
	go.etcd.io/bbolt v1.4.3
	golang.org/x/crypto v0.41.0
)

require (
	github.com/bits-and-blooms/bitset v1.24.0 // indirect
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/google/pprof v0.0.0-20250820193118-f64d9cf942d6 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ronanh/intcomp v1.1.1 // indirect
	github.com/rs/zerolog v1.34.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/sys v0.35.0 // indirect
)

replace gopkg.in/yaml.v3 => github.com/go-yaml/yaml v0.0.0-20220527083530-f6f7691b1fde
