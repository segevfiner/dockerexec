package dockerexec_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/segevfiner/dockerexec"
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

func TestCatGoodAndBadFile(t *testing.T) {
	// Testing combined output and error values.
	bs, err := dockerexec.Command(dockerClient, testImage, "cat", "/bogus/file.foo", "/etc/os-release").CombinedOutput()
	assert.IsType(t, &dockerexec.ExitError{}, err)

	sp := strings.SplitN(string(bs), "\n", 2)
	assert.Len(t, sp, 2)

	errLine, body := sp[0], sp[1]
	assert.True(t, strings.HasPrefix(errLine, "cat: /bogus/file.foo: No such file or directory"), errLine)
	assert.Contains(t, body, "Ubuntu")
}

func TestNoExistExecutable(t *testing.T) {
	// Can't run a non-existent executable
	err := dockerexec.Command(dockerClient, testImage, "/no-exist-executable").Run()
	assert.Error(t, err)
}

func TestExitStatus(t *testing.T) {
	// Test that exit values are returned correctly
	err := dockerexec.Command(dockerClient, testImage, "sh", "-c", "exit 42").Run()
	require.Error(t, err)

	if err, ok := err.(*dockerexec.ExitError); ok {
		assert.EqualError(t, err, "exit status 42")
		assert.Equal(t, int64(42), err.StatusCode)
	}
}

func TestExitCode(t *testing.T) {
	// Test that exit code are returned correctly
	cmd := dockerexec.Command(dockerClient, testImage, "sh", "-c", "exit 42")
	cmd.Run()
	assert.Equal(t, int64(42), cmd.StatusCode)

	cmd = dockerexec.Command(dockerClient, testImage, "sh", "-c", "exit 255")
	cmd.Run()
	assert.Equal(t, int64(255), cmd.StatusCode)

	cmd = dockerexec.Command(dockerClient, testImage, "cat")
	cmd.Run()
	assert.Equal(t, int64(0), cmd.StatusCode)

	// Test when command does not call Run().
	cmd = dockerexec.Command(dockerClient, testImage, "cat")
	assert.Equal(t, int64(-1), cmd.StatusCode)
}

func TestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := dockerexec.CommandContext(ctx, dockerClient, testImage, "sleep", "120")
	cmd.Start()
	cancel()
	err := cmd.Wait()
	assert.Error(t, err)
	assert.IsType(t, context.Canceled, err)
}

func TestNilContext(t *testing.T) {
	assert.Panics(t, func() {
		//lint:ignore SA1012 Test for panic
		dockerexec.CommandContext(nil, dockerClient, testImage, "cat")
	})
}

func TestCmdString(t *testing.T) {
	cmd := dockerexec.Command(dockerClient, testImage, "sh", "-c", "echo Hello, World!")
	assert.Equal(t, "sh -c echo Hello, World!", cmd.String())
}

func TestStartWait(t *testing.T) {
	cmd := dockerexec.Command(dockerClient, testImage, "sh", "-c", "echo Hello, World!")

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Start()
	require.NoError(t, err)

	err = cmd.Wait()
	require.NoError(t, err)

	assert.Equal(t, int64(0), cmd.StatusCode)
	assert.Equal(t, "Hello, World!\n", stdout.String())
}

func TestTty(t *testing.T) {
	cmd := dockerexec.Command(dockerClient, testImage, "sh", "-c", "tty")
	cmd.Config.Tty = true

	output, err := cmd.Output()
	require.NoError(t, err)
	assert.Contains(t, string(output), "/dev/pts")
}
