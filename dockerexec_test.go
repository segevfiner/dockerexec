package dockerexec_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/segevfiner/dockerexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testImage = "ubuntu:focal"

var dockerClient *client.Client

func TestMain(m *testing.M) {
	var err error

	dockerClient, err = client.NewClientWithOpts(client.WithAPIVersionNegotiation(), client.FromEnv)
	if err != nil {
		panic(err)
	}

	if _, _, err := dockerClient.ImageInspectWithRaw(context.Background(), testImage); err != nil {
		if client.IsErrNotFound(err) {
			pullOutput, err := dockerClient.ImagePull(context.Background(), testImage, types.ImagePullOptions{})
			if err != nil {
				panic(err)
			}
			defer pullOutput.Close()

			jsonmessage.DisplayJSONMessagesStream(pullOutput, os.Stderr, 0, false, nil)
		} else {
			panic(err)
		}
	}

	os.Exit(m.Run())
}

func TestEcho(t *testing.T) {
	cmd := dockerexec.Command(dockerClient, testImage, "sh", "-c", "echo Hello, World!")

	output, err := cmd.Output()
	require.NoError(t, err)

	assert.Equal(t, "Hello, World!\n", string(output))
}

func TestCatStdin(t *testing.T) {
	const input = "Line 1\nLine 2"
	cmd := dockerexec.Command(dockerClient, testImage, "cat")
	cmd.Stdin = strings.NewReader(input)

	output, err := cmd.Output()
	require.NoError(t, err)

	assert.Equal(t, input, string(output))
}

func TestCatFileRace(t *testing.T) {
	cmd := dockerexec.Command(dockerClient, testImage, "cat")

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)

	err = cmd.Start()
	require.NoError(t, err)

	wrote := make(chan bool)
	go func() {
		defer close(wrote)
		fmt.Fprint(stdin, "echo\n")
		stdin.Close()
	}()

	err = cmd.Wait()
	require.NoError(t, err)

	<-wrote
}
