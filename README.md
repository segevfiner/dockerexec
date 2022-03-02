dockerexec
==========
[![Go Reference](https://pkg.go.dev/badge/github.com/segevfiner/dockerexec.svg)](https://pkg.go.dev/github.com/segevfiner/dockerexec)
[![Build & Test](https://github.com/segevfiner/dockerexec/actions/workflows/go.yml/badge.svg)](https://github.com/segevfiner/dockerexec/actions/workflows/go.yml)

An "os/exec" like interface for running a command in a container, and being able to easily interact
with stdin, stdout, and other adjustments.

Usage
-----
```sh
$ go get github.com/segevfiner/dockerexec
```

Example:
```go
package main

import (
    "fmt"

    "github.com/docker/docker/client"
    "github.com/segevfiner/dockerexec"
)

func main() {
    // You might want to use client.WithVersion in production
    dockerClient, err := client.NewClientWithOpts(client.WithAPIVersionNegotiation(), client.FromEnv)
    if err != nil {
        panic(err)
    }

    cmd := dockerexec.Command(dockerClient, "ubuntu:focal", "sh", "-c", "echo Hello, World!")
    output, err := cmd.Output()
    if err != nil {
        panic(err)
    }

    fmt.Printf("%s", output)
}
```

License
-------
BSD-3-Clause.
