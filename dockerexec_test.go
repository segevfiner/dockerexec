package dockerexec_test

import (
	"context"
	"os"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/segevfiner/dockerexec"
	"github.com/stretchr/testify/assert"
)

const testImage = "ubuntu:focal"

var dockerClient *client.Client

func TestMain(m *testing.M) {
	var err error

	dockerClient, err = client.NewClientWithOpts(client.WithAPIVersionNegotiation(), client.FromEnv)
	if err != nil {
		panic(err)
	}

	pullOutput, err := dockerClient.ImagePull(context.Background(), testImage, types.ImagePullOptions{})
	if err != nil {
		panic(err)
	}
	defer pullOutput.Close()

	jsonmessage.DisplayJSONMessagesStream(pullOutput, os.Stderr, 0, false, nil)

	os.Exit(m.Run())
}

func TestEcho(t *testing.T) {
	cmd := dockerexec.Command(dockerClient, testImage, "sh", "-c", "echo Hello, World!")

	output, err := cmd.Output()
	assert.NoError(t, err)

	assert.Equal(t, "Hello, World!\n", string(output))
}
