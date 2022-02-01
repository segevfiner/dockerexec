package dockerexec_test

import (
	"fmt"

	"github.com/docker/docker/client"
	"github.com/segevfiner/dockerexec"
)

func Example() {
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
	// Output:
	// Hello, World!
}
