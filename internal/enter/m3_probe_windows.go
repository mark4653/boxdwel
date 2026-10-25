//go:build windows

package enter

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/mark4653/boxdwel/internal/m3probe"
)

// M3ProbePipeName is the fixed COM2 client pipe target for the transport spike.
const M3ProbePipeName = `\\.\pipe\vboxdistro-m3-probe`

// RunM3TransportProbe is the narrow M3 host experiment: reuse the M2 console
// pipe security/overlapped plumbing, announce readiness on stdout, accept the
// VM COM2 client within 30s, then run one bounded measurement with a fresh
// nonce. It runs no VBoxManage command and changes no host console mode.
func RunM3TransportProbe(out io.Writer) (m3probe.Report, error) {
	var rep m3probe.Report
	p, err := createConsolePipe(M3ProbePipeName)
	if err != nil {
		return rep, err
	}
	defer p.Close()
	if _, err := fmt.Fprintln(out, "M3_PIPE_READY"); err != nil {
		return rep, err
	}
	if err := p.Accept(30 * time.Second); err != nil {
		return rep, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return rep, err
	}
	cfg := m3probe.MeasureConfig{
		Nonce:   []byte(hex.EncodeToString(raw)),
		Timeout: 120 * time.Second,
	}
	return m3probe.Measure(p, cfg)
}
