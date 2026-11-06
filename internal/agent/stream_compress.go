package agent

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Stream compression for archives the agent writes (ClickHouse backups and
// move exports, file-set archives). Zstandard runs as the zstd tool at
// level 1 on every core (-1 -T0): an order of magnitude faster than Go's
// single-threaded gzip at the same size, and ClickHouse parts are already
// LZ4-compressed, so a higher level buys little. Without the tool the
// agent writes gzip (file sets) or plain tar (ClickHouse). Readers detect
// the compression from the stream's magic bytes, so archives of every
// kind and age restore the same way.

const (
	compressionNone = ""
	compressionZstd = "zstd"
	compressionGzip = "gzip"
)

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// zstdPath finds the zstd tool; tests replace it.
var zstdPath = func() (string, error) { return exec.LookPath("zstd") }

// zstdAvailable reports whether this agent can write and read Zstandard.
func zstdAvailable() bool {
	_, err := zstdPath()
	return err == nil
}

func zstdCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	path, err := zstdPath()
	if err != nil {
		return nil, errors.New("the zstd tool is not installed on this server; install the zstd package")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

// zstdWriter compresses everything written to it into w. Close flushes
// and waits for the tool; its error reports a failed compression.
type zstdWriter struct {
	stdin  io.WriteCloser
	cmd    *exec.Cmd
	stderr limitedBuffer
	once   sync.Once
	err    error
}

func newZstdWriter(ctx context.Context, w io.Writer) (*zstdWriter, error) {
	cmd, err := zstdCommand(ctx, "-1", "-T0", "-q", "-c")
	if err != nil {
		return nil, err
	}
	z := &zstdWriter{cmd: cmd}
	cmd.Stdout, cmd.Stderr = w, &z.stderr
	if z.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("the zstd tool could not be started")
	}
	return z, nil
}

func (z *zstdWriter) Write(p []byte) (int, error) { return z.stdin.Write(p) }

func (z *zstdWriter) Close() error {
	z.once.Do(func() {
		_ = z.stdin.Close()
		if err := z.cmd.Wait(); err != nil {
			z.err = errors.New("zstd compression failed: " + boundedErrorText(z.stderr.String(), 200))
		}
	})
	return z.err
}

// compressedWriter wraps w in the named compression; Close finishes the
// compressed stream but never closes w.
func compressedWriter(ctx context.Context, w io.Writer, compression string) (io.WriteCloser, error) {
	switch compression {
	case compressionZstd:
		return newZstdWriter(ctx, w)
	case compressionGzip:
		return gzip.NewWriterLevel(w, gzip.BestSpeed)
	case compressionNone:
		return nopWriteCloser{w}, nil
	}
	return nil, errors.New("unknown compression " + compression)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// decompressedReader detects the stream's compression (Zstandard, gzip or
// none) and returns its plain content. Close releases the decompressor
// and reports a stream that did not decompress cleanly.
func decompressedReader(ctx context.Context, r io.Reader) (io.ReadCloser, string, error) {
	buffered := bufio.NewReaderSize(r, 64<<10)
	head, _ := buffered.Peek(4)
	switch {
	case bytes.Equal(head, zstdMagic):
		cmd, err := zstdCommand(ctx, "-d", "-q", "-c")
		if err != nil {
			return nil, compressionZstd, err
		}
		cmd.Stdin = buffered
		var stderr limitedBuffer
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, compressionZstd, err
		}
		if err := cmd.Start(); err != nil {
			return nil, compressionZstd, errors.New("the zstd tool could not be started")
		}
		return &zstdReader{ReadCloser: out, cmd: cmd, stderr: &stderr}, compressionZstd, nil
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		zr, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, compressionGzip, errors.New("the archive is not a valid gzip stream")
		}
		return zr, compressionGzip, nil
	}
	return io.NopCloser(buffered), compressionNone, nil
}

type zstdReader struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *limitedBuffer
	once   sync.Once
	err    error
}

func (z *zstdReader) Close() error {
	z.once.Do(func() {
		// Drain so the tool can finish; a reader that stopped early is
		// cancelled through its context instead.
		_, _ = io.Copy(io.Discard, z.ReadCloser)
		if err := z.cmd.Wait(); err != nil {
			z.err = errors.New("zstd decompression failed: " + boundedErrorText(z.stderr.String(), 200))
		}
	})
	return z.err
}
