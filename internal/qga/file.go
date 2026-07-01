package qga

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// DefaultFileReadChunkSize is the default guest-agent file read chunk size.
const DefaultFileReadChunkSize = 1024 * 1024

// WriteFile writes a base64 payload to guestPath through client.
func WriteFile(client FileWriter, timeout time.Duration, guestPath string, payloadBase64 string) error {
	handle, err := client.OpenFile(timeout, guestPath)
	if err != nil {
		return err
	}

	writeErr := client.WriteFile(timeout, handle, payloadBase64)
	closeErr := client.CloseFile(timeout, handle)
	if writeErr != nil {
		if closeErr != nil {
			return errors.Join(writeErr, closeErr)
		}
		return writeErr
	}
	return closeErr
}

// ReadFile reads guestPath through client and decodes the base64 chunks.
func ReadFile(client FileReader, timeout time.Duration, guestPath string, chunkSize int) ([]byte, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultFileReadChunkSize
	}
	handle, err := client.OpenFileRead(timeout, guestPath)
	if err != nil {
		return nil, err
	}

	var result []byte
	for {
		payloadBase64, eof, readErr := client.ReadFile(timeout, handle, chunkSize)
		if readErr == nil && payloadBase64 != "" {
			chunk, decodeErr := base64.StdEncoding.DecodeString(payloadBase64)
			if decodeErr != nil {
				readErr = fmt.Errorf("decode guest file %q chunk: %w", guestPath, decodeErr)
			} else {
				result = append(result, chunk...)
			}
		}
		if readErr != nil {
			closeErr := client.CloseFile(timeout, handle)
			if closeErr != nil {
				return nil, errors.Join(readErr, closeErr)
			}
			return nil, readErr
		}
		if eof {
			break
		}
	}

	closeErr := client.CloseFile(timeout, handle)
	if closeErr != nil {
		return nil, closeErr
	}
	return result, nil
}
