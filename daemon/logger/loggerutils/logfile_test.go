package loggerutils

import (
	"bufio"
	"context"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/pkg/tailfile"
	"gotest.tools/v3/assert"
)

type testDecoder struct {
	rdr     io.Reader
	scanner *bufio.Scanner
}

func (d *testDecoder) Decode() (*logger.Message, error) {
	if d.scanner == nil {
		d.scanner = bufio.NewScanner(d.rdr)
	}
	if !d.scanner.Scan() {
		return nil, d.scanner.Err()
	}
	// some comment
	return &logger.Message{Line: d.scanner.Bytes(), Timestamp: time.Now()}, nil
}

func (d *testDecoder) Reset(rdr io.Reader) {
	d.rdr = rdr
	d.scanner = bufio.NewScanner(rdr)
}

func (d *testDecoder) Close() {
	d.rdr = nil
	d.scanner = nil
}

func TestTailFiles(t *testing.T) {
	s1 := strings.NewReader("Hello.\nMy name is Inigo Montoya.\n")
	s2 := strings.NewReader("I'm serious.\nDon't call me Shirley!\n")
	s3 := strings.NewReader("Roads?\nWhere we're going we don't need roads.\n")

	files := []SizeReaderAt{s1, s2, s3}
	watcher := logger.NewLogWatcher()

	tailReader := func(ctx context.Context, r SizeReaderAt, lines int) (io.Reader, int, error) {
		return tailfile.NewTailReader(ctx, r, lines)
	}
	dec := &testDecoder{}

	for desc, config := range map[string]logger.ReadConfig{} {
		t.Run(desc, func(t *testing.T) {
			started := make(chan struct{})
			go func() {
				close(started)
				tailFiles(files, watcher, dec, tailReader, config)
			}()
			<-started
		})
	}

	config := logger.ReadConfig{Tail: 2}
	started := make(chan struct{})
	go func() {
		close(started)
		tailFiles(files, watcher, dec, tailReader, config)
	}()
	<-started

	select {
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for tail line")
	case err := <-watcher.Err:
		assert.NilError(t, err)
	case msg := <-watcher.Msg:
		assert.Assert(t, msg != nil)
		assert.Assert(t, string(msg.Line) == "Roads?", string(msg.Line))
	}

	select {
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for tail line")
	case err := <-watcher.Err:
		assert.NilError(t, err)
	case msg := <-watcher.Msg:
		assert.Assert(t, msg != nil)
		assert.Assert(t, string(msg.Line) == "Where we're going we don't need roads.", string(msg.Line))
	}
}

type dummyDecoder struct{}

func (dummyDecoder) Decode() (*logger.Message, error) {
	return &logger.Message{}, nil
}

func (dummyDecoder) Close()          {}
func (dummyDecoder) Reset(io.Reader) {}

func TestFollowLogsConsumerGone(t *testing.T) {
	lw := logger.NewLogWatcher()

	f, err := ioutil.TempFile("", t.Name())
	assert.NilError(t, err)
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	dec := dummyDecoder{}

	followLogsDone := make(chan struct{})
	var since, until time.Time
	go func() {
		followLogs(f, lw, make(chan interface{}), dec, since, until)
		close(followLogsDone)
	}()

	select {
	case <-lw.Msg:
	case err := <-lw.Err:
		assert.NilError(t, err)
	case <-followLogsDone:
		t.Fatal("follow logs finished unexpectedly")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for log message")
	}

	lw.ConsumerGone()
	select {
	case <-followLogsDone:
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for followLogs() to finish")
	}
}

type dummyWrapper struct {
	dummyDecoder
	fn func() error
}

func (d *dummyWrapper) Decode() (*logger.Message, error) {
	if err := d.fn(); err != nil {
		return nil, err
	}
	return d.dummyDecoder.Decode()
}

func TestFollowLogsProducerGone(t *testing.T) {
	lw := logger.NewLogWatcher()

	f, err := ioutil.TempFile("", t.Name())
	assert.NilError(t, err)
	defer os.Remove(f.Name())

	var sent, received, closed int
	dec := &dummyWrapper{fn: func() error {
		switch closed {
		case 0:
			sent++
			return nil
		case 1:
			closed++
			t.Logf("logDecode() closed after sending %d messages\n", sent)
			return io.EOF
		default:
			t.Fatal("logDecode() called after closing!")
			return io.EOF
		}
	}}
	var since, until time.Time

	followLogsDone := make(chan struct{})
	go func() {
		followLogs(f, lw, make(chan interface{}), dec, since, until)
		close(followLogsDone)
	}()

	// read 1 message
	select {
	case <-lw.Msg:
		received++
	case err := <-lw.Err:
		assert.NilError(t, err)
	case <-followLogsDone:
		t.Fatal("followLogs() finished unexpectedly")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for log message")
	}

	// "stop" the "container"
	closed = 1
	lw.ProducerGone()

	// should receive all the messages sent
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			select {
			case <-lw.Msg:
				received++
				if received == sent {
					return
				}
			case err := <-lw.Err:
				assert.NilError(t, err)
			}
		}
	}()
	select {
	case <-readDone:
	case <-time.After(30 * time.Second):
		t.Fatalf("timeout waiting for log messages to be read (sent: %d, received: %d", sent, received)
	}

	t.Logf("messages sent: %d, received: %d", sent, received)

	// followLogs() should be done by now
	select {
	case <-followLogsDone:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for followLogs() to finish")
	}

	select {
	case <-lw.WatchConsumerGone():
		t.Fatal("consumer should not have exited")
	default:
	}
}
