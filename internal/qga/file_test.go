package qga

import (
	"errors"
	"testing"
	"time"
)

func TestWriteFileClosesHandleAfterWrite(t *testing.T) {
	client := &fileClient{openHandle: 42}
	if err := WriteFile(client, time.Second, "/tmp/file", "aGVsbG8="); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if client.wroteHandle != 42 || client.wrotePayload != "aGVsbG8=" {
		t.Fatalf("unexpected write: handle=%d payload=%q", client.wroteHandle, client.wrotePayload)
	}
	if client.closedHandle != 42 {
		t.Fatalf("expected handle close, got %d", client.closedHandle)
	}
}

func TestWriteFileJoinsWriteAndCloseErrors(t *testing.T) {
	writeErr := errors.New("write failed")
	closeErr := errors.New("close failed")
	client := &fileClient{openHandle: 42, writeErr: writeErr, closeErr: closeErr}
	err := WriteFile(client, time.Second, "/tmp/file", "payload")
	if !errors.Is(err, writeErr) || !errors.Is(err, closeErr) {
		t.Fatalf("expected joined errors, got %v", err)
	}
}

func TestReadFileReadsChunksAndClosesHandle(t *testing.T) {
	client := &fileClient{
		openHandle: 7,
		readChunks: []readChunk{
			{payload: "aGVs", eof: false},
			{payload: "bG8=", eof: true},
		},
	}
	data, err := ReadFile(client, time.Second, "/tmp/file", 2)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if got, want := string(data), "hello"; got != want {
		t.Fatalf("data: got %q want %q", got, want)
	}
	if client.closedHandle != 7 {
		t.Fatalf("expected handle close, got %d", client.closedHandle)
	}
	if client.readCount != 2 {
		t.Fatalf("read count: got %d want 2", client.readCount)
	}
}

func TestReadFileClosesHandleOnDecodeError(t *testing.T) {
	client := &fileClient{
		openHandle: 7,
		readChunks: []readChunk{
			{payload: "not base64", eof: true},
		},
	}
	_, err := ReadFile(client, time.Second, "/tmp/file", 1024)
	if err == nil {
		t.Fatalf("expected decode error")
	}
	if client.closedHandle != 7 {
		t.Fatalf("expected handle close, got %d", client.closedHandle)
	}
}

type readChunk struct {
	payload string
	eof     bool
	err     error
}

type fileClient struct {
	openHandle   int
	writeErr     error
	closeErr     error
	readChunks   []readChunk
	readCount    int
	wroteHandle  int
	wrotePayload string
	closedHandle int
}

func (c *fileClient) OpenFile(time.Duration, string) (int, error)     { return c.openHandle, nil }
func (c *fileClient) OpenFileRead(time.Duration, string) (int, error) { return c.openHandle, nil }
func (c *fileClient) ReadFile(time.Duration, int, int) (string, bool, error) {
	chunk := c.readChunks[c.readCount]
	c.readCount++
	return chunk.payload, chunk.eof, chunk.err
}
func (c *fileClient) WriteFile(_ time.Duration, handle int, payloadBase64 string) error {
	c.wroteHandle = handle
	c.wrotePayload = payloadBase64
	return c.writeErr
}
func (c *fileClient) CloseFile(_ time.Duration, handle int) error {
	c.closedHandle = handle
	return c.closeErr
}
