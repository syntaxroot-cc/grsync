// Package pipeline wires internal/sync (file enumeration, filtering,
// delta algorithm, attribute preservation) and internal/transport (framed
// subprocess/SSH connections) together into an actual sync. Neither of
// those packages imports the other - this package sits above both,
// importing them, so that layering stays intact.
package pipeline

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io"

	"github.com/syntaxroot-cc/grsync/internal/sync"
	"github.com/syntaxroot-cc/grsync/internal/transport"
)

// Encoding note: every message below is encoded with encoding/gob, not
// upstream rsync's actual wire protocol. That's a deliberate scope
// boundary, not an oversight: rsync's real wire format is an intricate,
// versioned binary protocol, and reimplementing it is a large, separate
// effort that doesn't belong inside this already-large integration
// ticket. gob is a reasonable, low-effort, *correct* choice specifically
// because grsync only ever talks to grsync here, never to real rsync -
// but true rsync protocol interoperability, if ever wanted, is future
// work, not something to let "protocol" quietly come to mean "gob."

// deltaOpKind tags which sync.DeltaOp variant a wireDeltaOp represents.
type deltaOpKind byte

const (
	deltaOpKindCopy deltaOpKind = iota
	deltaOpKindData
)

// wireDeltaOp is a wire-safe stand-in for sync.DeltaOp: DeltaOp is a
// sealed interface (CopyOp/DataOp), which gob cannot encode directly
// without registering concrete types with the encoder. Converting
// explicitly to/from this struct is more transparent than relying on
// gob's interface-registration machinery for just two variants.
type wireDeltaOp struct {
	Kind       deltaOpKind
	BlockIndex int    // valid when Kind == deltaOpKindCopy
	Bytes      []byte // valid when Kind == deltaOpKindData
}

func toWireDeltaOps(ops []sync.DeltaOp) ([]wireDeltaOp, error) {
	wire := make([]wireDeltaOp, len(ops))
	for i, op := range ops {
		switch o := op.(type) {
		case sync.CopyOp:
			wire[i] = wireDeltaOp{Kind: deltaOpKindCopy, BlockIndex: o.BlockIndex}
		case sync.DataOp:
			wire[i] = wireDeltaOp{Kind: deltaOpKindData, Bytes: o.Bytes}
		default:
			return nil, fmt.Errorf("op %d: unknown DeltaOp type %T", i, op)
		}
	}
	return wire, nil
}

func fromWireDeltaOps(wire []wireDeltaOp) ([]sync.DeltaOp, error) {
	ops := make([]sync.DeltaOp, len(wire))
	for i, w := range wire {
		switch w.Kind {
		case deltaOpKindCopy:
			ops[i] = sync.CopyOp{BlockIndex: w.BlockIndex}
		case deltaOpKindData:
			ops[i] = sync.DataOp{Bytes: w.Bytes}
		default:
			return nil, fmt.Errorf("op %d: unknown wire delta op kind %d", i, w.Kind)
		}
	}
	return ops, nil
}

// signatureMessage is FrameSignature's payload: one regular file's
// signature, tagged with its Path. Path is included even though both
// sides already process the file list in the same agreed-upon order -
// it's a cheap, valuable consistency check (see recvSignature/recvDelta)
// against a class of bug (an off-by-one, a dropped frame) that
// position-only encoding could never detect and would silently
// misapply one file's delta to another.
type signatureMessage struct {
	Path string
	Sig  sync.Signature
}

// deltaMessage is FrameDelta's payload: one regular file's delta ops,
// tagged with its Path for the same reason as signatureMessage.
type deltaMessage struct {
	Path string
	Ops  []wireDeltaOp
}

func encodeGob(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("gob encoding: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeGob(data []byte, v any) error {
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(v); err != nil {
		return fmt.Errorf("gob decoding: %w", err)
	}
	return nil
}

// readTypedFrame reads one frame from rw and confirms it has the
// expected type, translating a FrameError from the peer into a normal Go
// error along the way so a remote-side failure surfaces as an error here
// rather than a confusing "wrong frame type" mismatch.
func readTypedFrame(rw io.Reader, want transport.FrameType, what string) (transport.Frame, error) {
	f, err := transport.ReadFrame(rw)
	if err != nil {
		return transport.Frame{}, fmt.Errorf("reading %s: %w", what, err)
	}
	if f.Type == transport.FrameError {
		return transport.Frame{}, fmt.Errorf("remote error while awaiting %s: %s", what, f.Payload)
	}
	if f.Type != want {
		return transport.Frame{}, fmt.Errorf("expected %s (frame type %d), got frame type %d", what, want, f.Type)
	}
	return f, nil
}

// fileListMessage is FrameFileList's payload: the filtered file list plus
// which entries are hard-linked to each other on the source, so that
// grouping travels with the list itself rather than needing a separate
// round trip - HardLinkGroups is empty exactly when there's nothing to
// say about hard links, either because the source tree has none or
// because the sending platform can't detect them (sync.HardLinksSupported()
// is false).
type fileListMessage struct {
	Entries        []sync.FileEntry
	HardLinkGroups []sync.HardLinkGroup
}

func sendFileList(w io.Writer, entries []sync.FileEntry, groups []sync.HardLinkGroup) error {
	payload, err := encodeGob(fileListMessage{Entries: entries, HardLinkGroups: groups})
	if err != nil {
		return fmt.Errorf("encoding file list: %w", err)
	}
	return transport.WriteFrame(w, transport.Frame{Type: transport.FrameFileList, Payload: payload})
}

func recvFileList(r io.Reader) ([]sync.FileEntry, []sync.HardLinkGroup, error) {
	f, err := readTypedFrame(r, transport.FrameFileList, "file list")
	if err != nil {
		return nil, nil, err
	}
	var msg fileListMessage
	if err := decodeGob(f.Payload, &msg); err != nil {
		return nil, nil, fmt.Errorf("decoding file list: %w", err)
	}
	return msg.Entries, msg.HardLinkGroups, nil
}

func sendSignature(w io.Writer, path string, sig sync.Signature) error {
	payload, err := encodeGob(signatureMessage{Path: path, Sig: sig})
	if err != nil {
		return fmt.Errorf("encoding signature for %q: %w", path, err)
	}
	return transport.WriteFrame(w, transport.Frame{Type: transport.FrameSignature, Payload: payload})
}

func recvSignature(r io.Reader) (signatureMessage, error) {
	f, err := readTypedFrame(r, transport.FrameSignature, "signature")
	if err != nil {
		return signatureMessage{}, err
	}
	var msg signatureMessage
	if err := decodeGob(f.Payload, &msg); err != nil {
		return signatureMessage{}, fmt.Errorf("decoding signature: %w", err)
	}
	return msg, nil
}

func sendDelta(w io.Writer, path string, ops []sync.DeltaOp) error {
	wire, err := toWireDeltaOps(ops)
	if err != nil {
		return fmt.Errorf("converting delta for %q: %w", path, err)
	}
	payload, err := encodeGob(deltaMessage{Path: path, Ops: wire})
	if err != nil {
		return fmt.Errorf("encoding delta for %q: %w", path, err)
	}
	return transport.WriteFrame(w, transport.Frame{Type: transport.FrameDelta, Payload: payload})
}

func recvDelta(r io.Reader) (path string, ops []sync.DeltaOp, err error) {
	f, err := readTypedFrame(r, transport.FrameDelta, "delta")
	if err != nil {
		return "", nil, err
	}
	var msg deltaMessage
	if err := decodeGob(f.Payload, &msg); err != nil {
		return "", nil, fmt.Errorf("decoding delta: %w", err)
	}
	ops, err = fromWireDeltaOps(msg.Ops)
	if err != nil {
		return "", nil, fmt.Errorf("converting delta for %q: %w", msg.Path, err)
	}
	return msg.Path, ops, nil
}
