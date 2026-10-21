package terraform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// jsonOutput streams raw output while retaining complete error diagnostics and
// lock metadata. Memory grows with error diagnostics, not the full output.
type jsonOutput struct {
	diagnostics strings.Builder
	stdout      io.Writer
	writeErr    error
	line        []byte
	info        LockErrInfo
	found       bool
}

func (d *jsonOutput) Write(p []byte) (int, error) {
	n := len(p)
	// Keep draining Terraform even if the caller can no longer consume output.
	if d.stdout != nil && d.writeErr == nil {
		var written int
		written, d.writeErr = d.stdout.Write(p)
		if d.writeErr == nil && written != n {
			d.writeErr = io.ErrShortWrite
		}
	}
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			d.line = append(d.line, p...)
			break
		}
		if len(d.line) == 0 {
			d.record(p[:end])
		} else {
			d.line = append(d.line, p[:end]...)
			d.flush()
		}
		p = p[end+1:]
	}
	return n, nil
}

func (d *jsonOutput) flush() {
	d.record(d.line)
	d.line = nil
}

func (d *jsonOutput) record(line []byte) {
	if len(line) == 0 {
		return
	}
	var event struct {
		Type       string `json:"type"`
		Diagnostic *struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
		} `json:"diagnostic"`
	}
	if json.Unmarshal(line, &event) != nil || event.Type != "diagnostic" || event.Diagnostic == nil {
		return
	}
	if event.Diagnostic.Severity != "error" {
		return
	}
	// Keep the whole record, including source context and fields unknown to us.
	d.diagnostics.Write(line)
	d.diagnostics.WriteByte('\n')
	if !d.found && event.Diagnostic.Summary == "Error acquiring the state lock" {
		d.info, d.found = parseStateLockInfo(event.Diagnostic.Detail)
	}
}

// result preserves the original process error and any output failure, adding
// diagnostics only when the attempt failed. Successful retries remain successful.
func (d *jsonOutput) result(processErr error) error {
	if d.writeErr != nil {
		processErr = errors.Join(processErr, d.writeErr)
	}
	if processErr != nil && d.diagnostics.Len() > 0 {
		return fmt.Errorf("%w\n%s", processErr, d.diagnostics.String())
	}
	return processErr
}
