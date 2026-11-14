package credential

import "errors"

// ErrDeliveryBlocked is returned by SettleSubmit when redaction set a
// delivery_blocked class; the class is in the RedactResult.
var ErrDeliveryBlocked = errors.New("credential: delivery blocked by redaction")

// SettleSubmit is the credential gate in front of any submit (AC-18 with
// AC-15/AC-16): it loads the attempt's bound matcher, redacts the raw
// streams into outDir, and only then calls submit with the redacted
// result. submit is never called when LoadBound fails
// (credential_material_unavailable or credential_rotated_mid_attempt),
// when redaction fails, or when redaction blocks delivery. The caller
// (W2's settle) records the returned error's class and calls MarkSettled
// in every case, so GC may proceed after a fail-closed settle.
func (p Policy) SettleSubmit(bindingsDir, attempt string, idKey []byte, current []OtherCredential,
	rawStdout, rawStderr, outDir string, submit func(*Bound, RedactResult) error) (RedactResult, error) {
	b, err := p.LoadBound(bindingsDir, attempt, idKey, current)
	if err != nil {
		return RedactResult{}, err
	}
	res, err := b.RedactStreams(rawStdout, rawStderr, outDir)
	if err != nil {
		return RedactResult{}, err
	}
	if res.Blocked != "" {
		return res, ErrDeliveryBlocked
	}
	return res, submit(b, res)
}
