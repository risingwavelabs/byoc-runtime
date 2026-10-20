package terraform

import (
	"bytes"
	"encoding/json"
	"io"
)

// jsonStateLockDetector consumes JSON records and retains only lock metadata.
// terraform-exec already buffers one whole record; do not retain the full output.
// Raw output and process errors are handled by the caller without modification.
type jsonStateLockDetector struct {
	stdout   io.Writer
	writeErr error
	line     []byte
	info     LockErrInfo
	found    bool
}

func (d *jsonStateLockDetector) Write(p []byte) (int, error) {
	n := len(p)
	// Keep draining Terraform even if the caller can no longer consume output.
	if d.stdout != nil && d.writeErr == nil {
		var written int
		written, d.writeErr = d.stdout.Write(p)
		if d.writeErr == nil && written != n {
			d.writeErr = io.ErrShortWrite
		}
	}
	for len(p) > 0 && !d.found {
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

func (d *jsonStateLockDetector) flush() {
	d.record(d.line)
	d.line = nil
}

func (d *jsonStateLockDetector) record(line []byte) {
	if d.found || len(line) == 0 {
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
	if diagnostic := event.Diagnostic; diagnostic.Severity == "error" && diagnostic.Summary == "Error acquiring the state lock" {
		d.info, d.found = parseStateLockInfo(diagnostic.Detail)
	}
}
