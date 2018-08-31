// Copyright 2015-2018 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/gnatsd/server"
	"github.com/nats-io/go-nats"

	gnatsd "github.com/nats-io/gnatsd/test"
)

// So that we can pass tests and benchmarks...
type tLogger interface {
	Fatalf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

// TestLogger
type TestLogger tLogger

// Dumb wait program to sync on callbacks, etc... Will timeout
func Wait(ch chan bool) error {
	return WaitTime(ch, 5*time.Second)
}

// Wait for a chan with a timeout.
func WaitTime(ch chan bool, timeout time.Duration) error {
	select {
	case <-ch:
		return nil
	case <-time.After(timeout):
	}
	return errors.New("timeout")
}

func stackFatalf(t tLogger, f string, args ...interface{}) {
	lines := make([]string, 0, 32)
	msg := fmt.Sprintf(f, args...)
	lines = append(lines, msg)

	// Generate the Stack of callers: Skip us and verify* frames.
	for i := 1; true; i++ {
		_, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		msg := fmt.Sprintf("%d - %s:%d", i, file, line)
		lines = append(lines, msg)
	}
	t.Fatalf("%s", strings.Join(lines, "\n"))
}

////////////////////////////////////////////////////////////////////////////////
// Creating client connections
////////////////////////////////////////////////////////////////////////////////

// NewDefaultConnection
func NewDefaultConnection(t tLogger) *nats.Conn {
	return NewConnection(t, nats.DefaultPort)
}

// NewConnection forms connection on a given port.
func NewConnection(t tLogger, port int) *nats.Conn {
	url := fmt.Sprintf("nats://localhost:%d", port)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("Failed to create default connection: %v\n", err)
		return nil
	}
	return nc
}

// NewEConn
func NewEConn(t tLogger) *nats.EncodedConn {
	ec, err := nats.NewEncodedConn(NewDefaultConnection(t), nats.DEFAULT_ENCODER)
	if err != nil {
		t.Fatalf("Failed to create an encoded connection: %v\n", err)
	}
	return ec
}

////////////////////////////////////////////////////////////////////////////////
// Running gnatsd server in separate Go routines
////////////////////////////////////////////////////////////////////////////////

// RunDefaultServer will run a server on the default port.
func RunDefaultServer() *server.Server {
	return RunServerOnPort(nats.DefaultPort)
}

// RunServerOnPort will run a server on the given port.
func RunServerOnPort(port int) *server.Server {
	opts := gnatsd.DefaultTestOptions
	opts.Port = port
	return RunServerWithOptions(opts)
}

// RunServerWithOptions will run a server with the given options.
func RunServerWithOptions(opts server.Options) *server.Server {
	return gnatsd.RunServer(&opts)
}

// RunServerWithConfig will run a server with the given configuration file.
func RunServerWithConfig(configFile string) (*server.Server, *server.Options) {
	return gnatsd.RunServerWithConfig(configFile)
}

type wintercept func(io.WriteCloser) io.Writer

func passthough(w io.WriteCloser) io.Writer {
	return w
}

func block(ch <-chan time.Duration) wintercept {
	return func(w io.WriteCloser) io.Writer {
		return &blocker{ch: ch, w: w}
	}
}

type blocker struct {
	ch <-chan time.Duration
	w  io.Writer
}

func (b *blocker) Write(p []byte) (int, error) {
	select {
	case d, ok := <-b.ch:
		if !ok {
			return 0, io.ErrClosedPipe
		}
		time.Sleep(d)
	default:
	}
	return b.w.Write(p)
}

type proxy struct {
	t    *testing.T
	ln   net.Listener
	up   string
	quit chan struct{}
	dw   wintercept
	uw   wintercept
}

func newProxy(t *testing.T, targetAddr string, dw, uw wintercept) (*proxy, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxy{
		t:    t,
		up:   targetAddr,
		ln:   ln,
		dw:   dw,
		uw:   uw,
		quit: make(chan struct{}),
	}

	go p.acceptLoop()

	return p, nil
}

func (p *proxy) Addr() net.Addr {
	return p.ln.Addr()
}

func (p *proxy) copy(down, up net.Conn) {
	done := make(chan struct{}, 1)
	go func() {
		io.Copy(p.dw(down), up)
		select {
		case done <- struct{}{}:
		default:
		}
	}()
	go func() {
		io.Copy(p.uw(up), down)
		select {
		case done <- struct{}{}:
		default:
		}
	}()

	select {
	case <-done:
	case <-p.quit:
	}
	up.Close()
	down.Close()
}

func (p *proxy) acceptLoop() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}

		down.(*net.TCPConn).SetReadBuffer(1024)

		up, err := net.DialTimeout("tcp", p.up, 2*time.Second)
		if err != nil {
			p.t.Fatal(err)
		}
		go p.copy(down, up)
	}
}

func (p *proxy) Close() error {
	err := p.ln.Close()
	close(p.quit)
	return err
}
