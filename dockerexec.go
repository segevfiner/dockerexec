// Package dockerexec runs a command in a container. It wraps the Docker API to make it easier to
// remap stdin and stdout, connect I/O with pipes, and do other adjustments.
//
// This is essentially an "os/exec" like interface to running a command in a container.
package dockerexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Cmd represents a container being prepared or run.
//
// A Cmd cannot be reused after calling its Run, Output or CombinedOutput
// methods.
type Cmd struct {
	// The configuration of the container to be ran.
	//
	// Some properties are handled specially:
	// 		* AutoRemove default to true.
	//		* StdinOnce defaults to true, and you should be careful unsetting it (https://github.com/moby/moby/issues/38457).
	//		* OpenStdin will be set automatically as needed.
	Config           *container.Config
	HostConfig       *container.HostConfig
	Networkingconfig *network.NetworkingConfig
	Platform         *specs.Platform
	ContainerName    string

	// Stdin specifies the container's standard input.
	//
	// If Stdin is nil, the container will receive no input.
	//
	// During the execution of the command a separate goroutine reads from Stdin and
	// delivers that data to the container. In this case, Wait does not complete until the goroutine
	// stops copying, either because it has reached the end of Stdin (EOF or a read error) or
	// because writing to the container.
	Stdin io.Reader

	// Stdout and Stderr specify the process's standard output and error.
	//
	// If either is nil, the corresponding output will be discarded.
	//
	// While using Config.Tty, only a single stream of output is available in Stdout. Setting Stderr
	// will panic.
	//
	// During the execution of the command a separate goroutine reads from the container and
	// delivers that data to the corresponding Writer. In this case, Wait does not complete until
	// the goroutine reaches EOF or encounters an error.
	Stdout io.Writer
	Stderr io.Writer

	// TODO Add callback BeforeStart (For users that want to start stats or event monitoring)

	// TODO "os/exec" has an os.Process object, which also has methods to Kill & Wait, etc.
	// We also don't support starting a detached container with this API which in plain "os/exec" is
	// done by directly calling os.StartProcess

	// ContainerID is the ID of the container, once started.
	ContainerID string

	// Warnings contains any warnings from creating the container.
	//
	// You should consider logging these.
	Warnings []string

	// StatusCode contains the status code of the container, available after a call to Wait or Run.
	StatusCode int64

	ctx              context.Context // nil means None
	cli              client.APIClient
	finished         bool // when Wait was called
	closeAfterWait   []io.Closer
	closeAfterStdin  []io.Closer
	closeAfterOutput []io.Closer
	goroutine        []func() error
	errch            chan error // one send per goroutine
	waitCh           <-chan container.ContainerWaitOKBody
	waitErrCh        <-chan error
	waitDone         chan struct{}
}

// Command returns the Cmd struct to execute the named program inside the given image with the given
// arguments.
func Command(cli client.APIClient, image string, name string, arg ...string) *Cmd {
	return &Cmd{
		Config: &container.Config{
			Image:     image,
			Cmd:       append([]string{name}, arg...),
			StdinOnce: true,
		},
		HostConfig: &container.HostConfig{
			AutoRemove: true,
		},

		cli: cli,
	}
}

// CommandContext is like Command but includes a context.
//
// The provided context is used to kill the container (by calling
// ContainerKill) if the context becomes done before the container
// completes on its own.
func CommandContext(ctx context.Context, cli client.APIClient, image string, name string, arg ...string) *Cmd {
	if ctx == nil {
		panic("nil Context")
	}
	cmd := Command(cli, image, name, arg...)
	cmd.ctx = ctx
	return cmd
}

// String returns a human-readable description of c.
// It is intended only for debugging.
func (c *Cmd) String() string {
	b := new(strings.Builder)
	for _, a := range c.Config.Entrypoint {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(a)
	}
	for _, a := range c.Config.Cmd {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(a)
	}
	return b.String()
}

func (c *Cmd) closeDescriptors(closers []io.Closer) {
	for _, fd := range closers {
		fd.Close()
	}
}

// Run starts the specified container and waits for it to complete.
//
// The returned error is nil if the container runs, has no problems
// copying stdin, stdout, and stderr, and exits with a zero exit
// status.
//
// If the container starts but does not complete successfully, the error is of
// type *ExitError. Other error types may be returned for other situations.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

func (c *Cmd) stdin(attach types.HijackedResponse) {
	c.goroutine = append(c.goroutine, func() error {
		_, err := io.Copy(attach.Conn, c.Stdin)
		if err1 := attach.CloseWrite(); err == nil {
			err = err1
		}
		c.closeDescriptors(c.closeAfterStdin)
		return err
	})
}

func (c *Cmd) stdoutStderr(attach types.HijackedResponse) {
	c.goroutine = append(c.goroutine, func() error {
		stdout := c.Stdout
		if stdout == nil {
			stdout = io.Discard
		}

		stderr := c.Stderr
		if stderr == nil {
			stderr = io.Discard
		}

		var err error
		if c.Config.Tty {
			_, err = io.Copy(stdout, attach.Reader)
		} else {
			_, err = stdcopy.StdCopy(stdout, stderr, attach.Reader)
		}

		c.closeDescriptors(c.closeAfterOutput)

		return err
	})
}

// Start starts the specified container but does not wait for it to complete.
//
// If Start returns successfully, the c.ContainerID field will be set.
//
// The Wait method will return the exit code and release associated resources
// once the command exits.
func (c *Cmd) Start() error {
	if len(c.ContainerID) != 0 {
		return errors.New("dockerexec: already started")
	}
	if c.Config.Tty && c.Stderr != nil {
		return errors.New("dockerexec: can't set both Config.Tty and Stderr")
	}

	var ctx context.Context
	if c.ctx != nil {
		ctx = c.ctx

		select {
		case <-c.ctx.Done():
			c.closeDescriptors(c.closeAfterStdin)
			c.closeDescriptors(c.closeAfterOutput)
			c.closeDescriptors(c.closeAfterWait)
			return c.ctx.Err()
		default:
		}
	} else {
		ctx = context.Background()
	}

	cont, err := c.cli.ContainerCreate(
		ctx,
		c.Config,
		c.HostConfig,
		c.Networkingconfig,
		c.Platform,
		c.ContainerName,
	)
	if err != nil {
		c.closeDescriptors(c.closeAfterStdin)
		c.closeDescriptors(c.closeAfterOutput)
		c.closeDescriptors(c.closeAfterWait)
		return err
	}

	c.Warnings = cont.Warnings

	attach, err := c.cli.ContainerAttach(ctx, cont.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  c.Stdin != nil,
		Stdout: c.Stdout != nil,
		Stderr: c.Stderr != nil,
	})
	if err != nil {
		c.closeDescriptors(c.closeAfterStdin)
		c.closeDescriptors(c.closeAfterOutput)
		c.closeDescriptors(c.closeAfterWait)
		_ = c.cli.ContainerRemove(context.Background(), cont.ID, types.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		})
		return err
	}

	if c.Stdin != nil {
		c.Config.OpenStdin = true
		c.stdin(attach)
	}

	if c.Stdout != nil || c.Stderr != nil {
		c.stdoutStderr(attach)
	}

	c.waitCh, c.waitErrCh = c.cli.ContainerWait(ctx, cont.ID, container.WaitConditionNextExit)

	err = c.cli.ContainerStart(ctx, cont.ID, types.ContainerStartOptions{})
	if err != nil {
		c.closeDescriptors(c.closeAfterStdin)
		c.closeDescriptors(c.closeAfterOutput)
		c.closeDescriptors(c.closeAfterWait)
		_ = c.cli.ContainerRemove(context.Background(), cont.ID, types.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		})
		return err
	}

	c.ContainerID = cont.ID

	// Don't allocate the channel unless there are goroutines to fire.
	if len(c.goroutine) > 0 {
		c.errch = make(chan error, len(c.goroutine))
		for _, fn := range c.goroutine {
			go func(fn func() error) {
				c.errch <- fn()
			}(fn)
		}
	}

	if c.ctx != nil {
		c.waitDone = make(chan struct{})
		go func() {
			select {
			case <-c.ctx.Done():
				// TODO Graceful termination? Add kill method?
				_ = c.cli.ContainerKill(context.Background(), cont.ID, "SIGKILL")
			case <-c.waitDone:
			}
		}()
	}

	return nil
}

// An ExitError reports an unsuccessful exit by a container.
type ExitError struct {
	StatusCode int64

	// Stderr holds a subset of the standard error output from the
	// Cmd.Output method if standard error was not otherwise being
	// collected.
	//
	// If the error output is long, Stderr may contain only a prefix
	// and suffix of the output, with the middle replaced with
	// text about the number of omitted bytes.
	//
	// Stderr is provided for debugging, for inclusion in error messages.
	// Users with other needs should redirect Cmd.Stderr as needed.
	Stderr []byte
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.StatusCode)
}

// Wait waits for the container to exit and waits for any copying to
// stdin or copying from stdout or stderr to complete.
//
// The container must have been started by Start.
//
// The returned error is nil if the container runs, has no problems
// copying stdin, stdout, and stderr, and exits with a zero exit
// status.
//
// If the container fails to run or doesn't complete successfully, the
// error is of type *ExitError. Other error types may be
// returned for I/O problems.
//
// Wait also waits for the respective I/O loop copying to or from the process to complete.
//
// Wait releases any resources associated with the Cmd.
func (c *Cmd) Wait() error {
	var err error

	if len(c.ContainerID) == 0 {
		return errors.New("dockerexec: not started")
	}
	if c.finished {
		return errors.New("dockerexec: Wait was already called")
	}

	select {
	case waitResult := <-c.waitCh:
		if waitResult.Error != nil {
			err = errors.New(waitResult.Error.Message)
		}

		c.StatusCode = waitResult.StatusCode
	case err = <-c.waitErrCh:
	}
	if c.waitDone != nil {
		close(c.waitDone)
	}

	var copyError error
	for range c.goroutine {
		if err := <-c.errch; err != nil && copyError == nil {
			copyError = err
		}
	}

	c.closeDescriptors(c.closeAfterWait)

	if err != nil {
		return err
	} else if c.StatusCode != 0 {
		return &ExitError{StatusCode: c.StatusCode}
	}

	return copyError
}

// Output runs the container and returns its standard output.
// Any returned error will usually be of type *ExitError.
// If c.Stderr was nil, Output populates ExitError.Stderr.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("dockerexec: Stdout already set")
	}
	var stdout bytes.Buffer
	c.Stdout = &stdout

	captureErr := c.Stderr == nil
	if captureErr {
		c.Stderr = &prefixSuffixSaver{N: 32 << 10}
	}

	err := c.Run()
	if err != nil && captureErr {
		if ee, ok := err.(*ExitError); ok {
			ee.Stderr = c.Stderr.(*prefixSuffixSaver).Bytes()
		}
	}
	return stdout.Bytes(), err
}

// CombinedOutput runs the container and returns its combined standard
// output and standard error.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("dockerexec: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("dockerexec: Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b
	err := c.Run()
	return b.Bytes(), err
}

// StdinPipe returns a pipe that will be connected to the container's
// standard input when the container starts.
// The pipe will be closed automatically after Wait sees the command exit.
// A caller need only call Close to force the pipe to close sooner.
// For example, if the command being run will not exit until standard input
// is closed, the caller must close the pipe.
func (c *Cmd) StdinPipe() (io.WriteCloser, error) {
	if c.Stdin != nil {
		return nil, errors.New("dockerexec: Stdin already set")
	}
	if len(c.ContainerID) != 0 {
		return nil, errors.New("dockerexec: StdinPipe after container started")
	}
	pr, pw := io.Pipe()
	c.Stdin = pr
	c.closeAfterStdin = append(c.closeAfterStdin, pr)
	wc := &closeOnce{PipeWriter: pw}
	c.closeAfterWait = append(c.closeAfterWait, wc)
	return wc, nil
}

type closeOnce struct {
	*io.PipeWriter

	once sync.Once
	err  error
}

func (c *closeOnce) Close() error {
	c.once.Do(c.close)
	return c.err
}

func (c *closeOnce) close() {
	c.err = c.PipeWriter.Close()
}

// StdoutPipe returns a pipe that will be connected to the container's
// standard output when the container starts.
//
// Wait will close the pipe after seeing the container exit, so most callers
// need not close the pipe themselves. It is thus incorrect to call Wait
// before all reads from the pipe have completed.
// For the same reason, it is incorrect to call Run when using StdoutPipe.
// See the example for idiomatic usage.
func (c *Cmd) StdoutPipe() (io.ReadCloser, error) {
	if c.Stdout != nil {
		return nil, errors.New("dockerexec: Stdout already set")
	}
	if len(c.ContainerID) != 0 {
		return nil, errors.New("dockerexec: StdoutPipe after container started")
	}
	pr, pw := io.Pipe()
	c.Stdout = pw
	c.closeAfterOutput = append(c.closeAfterOutput, pw)
	c.closeAfterWait = append(c.closeAfterWait, pr)
	return pr, nil
}

// StderrPipe returns a pipe that will be connected to the container's
// standard error when the container starts.
//
// Wait will close the pipe after seeing the container exit, so most callers
// need not close the pipe themselves. It is thus incorrect to call Wait
// before all reads from the pipe have completed.
// For the same reason, it is incorrect to use Run when using StderrPipe.
// See the StdoutPipe example for idiomatic usage.
func (c *Cmd) StderrPipe() (io.ReadCloser, error) {
	if c.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	if len(c.ContainerID) != 0 {
		return nil, errors.New("exec: StderrPipe after container started")
	}
	pr, pw := io.Pipe()
	c.Stderr = pw
	c.closeAfterOutput = append(c.closeAfterOutput, pw)
	c.closeAfterWait = append(c.closeAfterWait, pr)
	return pr, nil
}

// prefixSuffixSaver is an io.Writer which retains the first N bytes
// and the last N bytes written to it. The Bytes() methods reconstructs
// it with a pretty error message.
type prefixSuffixSaver struct {
	N         int // max size of prefix or suffix
	prefix    []byte
	suffix    []byte // ring buffer once len(suffix) == N
	suffixOff int    // offset to write into suffix
	skipped   int64

	// TODO(bradfitz): we could keep one large []byte and use part of it for
	// the prefix, reserve space for the '... Omitting N bytes ...' message,
	// then the ring buffer suffix, and just rearrange the ring buffer
	// suffix when Bytes() is called, but it doesn't seem worth it for
	// now just for error messages. It's only ~64KB anyway.
}

func (w *prefixSuffixSaver) Write(p []byte) (n int, err error) {
	lenp := len(p)
	p = w.fill(&w.prefix, p)

	// Only keep the last w.N bytes of suffix data.
	if overage := len(p) - w.N; overage > 0 {
		p = p[overage:]
		w.skipped += int64(overage)
	}
	p = w.fill(&w.suffix, p)

	// w.suffix is full now if p is non-empty. Overwrite it in a circle.
	for len(p) > 0 { // 0, 1, or 2 iterations.
		n := copy(w.suffix[w.suffixOff:], p)
		p = p[n:]
		w.skipped += int64(n)
		w.suffixOff += n
		if w.suffixOff == w.N {
			w.suffixOff = 0
		}
	}
	return lenp, nil
}

// fill appends up to len(p) bytes of p to *dst, such that *dst does not
// grow larger than w.N. It returns the un-appended suffix of p.
func (w *prefixSuffixSaver) fill(dst *[]byte, p []byte) (pRemain []byte) {
	if remain := w.N - len(*dst); remain > 0 {
		add := minInt(len(p), remain)
		*dst = append(*dst, p[:add]...)
		p = p[add:]
	}
	return p
}

func (w *prefixSuffixSaver) Bytes() []byte {
	if w.suffix == nil {
		return w.prefix
	}
	if w.skipped == 0 {
		return append(w.prefix, w.suffix...)
	}
	var buf bytes.Buffer
	buf.Grow(len(w.prefix) + len(w.suffix) + 50)
	buf.Write(w.prefix)
	buf.WriteString("\n... omitting ")
	buf.WriteString(strconv.FormatInt(w.skipped, 10))
	buf.WriteString(" bytes ...\n")
	buf.Write(w.suffix[w.suffixOff:])
	buf.Write(w.suffix[:w.suffixOff])
	return buf.Bytes()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
