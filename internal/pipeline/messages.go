// Package pipeline wires internal/sync (file enumeration, filtering,
// delta algorithm, attribute preservation) and internal/transport (framed
// subprocess/SSH connections) together into an actual sync.
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
// upstream rsync's actual wire protocol - a deliberate scope boundary,
// since grsync only ever talks to grsync here.

// deltaOpKind tags which sync.DeltaOp variant a wireDeltaOp represents.
type deltaOpKind byte

const (
	deltaOpKindCopy deltaOpKind = iota
	deltaOpKindData
)

// wireDeltaOp is a wire-safe stand-in for sync.DeltaOp: DeltaOp is a
// sealed interface (CopyOp/DataOp), which gob can't encode directly
// without registering concrete types.
type wireDeltaOp struct {
	Kind       deltaOpKind
	BlockIndex int    // valid when Kind == deltaOpKindCopy
	Bytes      []byte // valid when Kind == deltaOpKindData and the enclosing deltaMessage is NOT compressed
	Length     int    // valid when Kind == deltaOpKindData and the enclosing deltaMessage IS compressed: how many bytes of its decompressed Literal stream belong to this op
}

// toWireDeltaOps converts ops to their wire form, compressing this file's
// entire literal-data stream as a single zlib unit when copts calls for
// it. CopyOp block indices are never compressed, since they're plain
// integers, not data.
func toWireDeltaOps(ops []sync.DeltaOp, path string, copts CompressOptions) (wire []wireDeltaOp, compressed bool, literal []byte, err error) {
	wire = make([]wireDeltaOp, len(ops))

	tryCompress := copts.Enabled && !skipCompressSuffix(path, copts.SkipSuffixes)
	var concatenated []byte
	if tryCompress {
		for _, op := range ops {
			if d, ok := op.(sync.DataOp); ok {
				concatenated = append(concatenated, d.Bytes...)
			}
		}
		if len(concatenated) > 0 {
			if c, ok := compressLiteral(concatenated, copts.Level); ok {
				literal = c
				compressed = true
			}
		}
	}

	for i, op := range ops {
		switch o := op.(type) {
		case sync.CopyOp:
			wire[i] = wireDeltaOp{Kind: deltaOpKindCopy, BlockIndex: o.BlockIndex}
		case sync.DataOp:
			if compressed {
				wire[i] = wireDeltaOp{Kind: deltaOpKindData, Length: len(o.Bytes)}
			} else {
				wire[i] = wireDeltaOp{Kind: deltaOpKindData, Bytes: o.Bytes}
			}
		default:
			return nil, false, nil, fmt.Errorf("op %d: unknown DeltaOp type %T", i, op)
		}
	}
	return wire, compressed, literal, nil
}

// fromWireDeltaOps reverses toWireDeltaOps: when compressed is true, it
// decompresses literal once and re-slices it back into each op's bytes
// using the Length each wireDeltaOp carried.
func fromWireDeltaOps(wire []wireDeltaOp, compressed bool, literal []byte) ([]sync.DeltaOp, error) {
	var decompressed []byte
	if compressed {
		var err error
		decompressed, err = decompressLiteral(literal)
		if err != nil {
			return nil, fmt.Errorf("decompressing literal data: %w", err)
		}
	}

	ops := make([]sync.DeltaOp, len(wire))
	pos := 0
	for i, w := range wire {
		switch w.Kind {
		case deltaOpKindCopy:
			ops[i] = sync.CopyOp{BlockIndex: w.BlockIndex}
		case deltaOpKindData:
			if !compressed {
				ops[i] = sync.DataOp{Bytes: w.Bytes}
				continue
			}
			end := pos + w.Length
			if w.Length < 0 || end > len(decompressed) {
				return nil, fmt.Errorf("op %d: decompressed literal stream too short (want %d more bytes at offset %d, have %d total)", i, w.Length, pos, len(decompressed))
			}
			ops[i] = sync.DataOp{Bytes: decompressed[pos:end]}
			pos = end
		default:
			return nil, fmt.Errorf("op %d: unknown wire delta op kind %d", i, w.Kind)
		}
	}
	return ops, nil
}

// signatureMessage is FrameSignature's payload: one regular file's
// signature, tagged with Path as a consistency check against an
// off-by-one or dropped frame silently misapplying one file's delta to
// another.
//
// Append tells Sender how to respond to this signature, and defaults to
// appendNone (gob's zero value), so a signatureMessage sent before
// --append existed still decodes correctly.
type signatureMessage struct {
	Path   string
	Sig    sync.Signature
	Append appendAction
}

// appendAction is carried on a signatureMessage to tell Sender how to
// respond to it.
type appendAction byte

const (
	// appendNone is the normal flow: Sender runs sync.GenerateDelta
	// against Sig as usual.
	appendNone appendAction = iota
	// appendTail means the receiver's existing Sig.BlockSize bytes are
	// trusted unverified; only the literal tail past that offset is sent.
	// Sig.BlockSize carries that trusted offset, not a real block size;
	// Sig.Blocks is unused.
	appendTail
	// appendSkip means the destination is already at least as long as the
	// source: acknowledge with an empty delta without reading or
	// comparing anything.
	appendSkip
)

// deltaMessage is FrameDelta's payload: one regular file's delta ops,
// tagged with Path for the same reason as signatureMessage.
//
// Literal holds every DataOp's bytes for this file, zlib-compressed
// together as a single stream when Compressed is true, amortizing zlib's
// fixed header/trailer overhead across the whole file instead of paying
// it per op; each wireDeltaOp's Length then says how many of Literal's
// decompressed bytes are its own. When Compressed is false, Literal is
// unused and every op carries its own Bytes directly.
type deltaMessage struct {
	Path       string
	Ops        []wireDeltaOp
	Compressed bool
	Literal    []byte
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

// readTypedFrame reads one frame from rw and confirms it has the expected
// type, translating a peer's FrameError into a normal Go error.
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
// which entries are hard-linked to each other, so the grouping travels
// with the list instead of needing a separate round trip.
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

func sendSignature(w io.Writer, path string, sig sync.Signature, action appendAction) error {
	payload, err := encodeGob(signatureMessage{Path: path, Sig: sig, Append: action})
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

func sendDelta(w io.Writer, path string, ops []sync.DeltaOp, copts CompressOptions) error {
	wire, compressed, literal, err := toWireDeltaOps(ops, path, copts)
	if err != nil {
		return fmt.Errorf("converting delta for %q: %w", path, err)
	}
	payload, err := encodeGob(deltaMessage{Path: path, Ops: wire, Compressed: compressed, Literal: literal})
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
	ops, err = fromWireDeltaOps(msg.Ops, msg.Compressed, msg.Literal)
	if err != nil {
		return "", nil, fmt.Errorf("converting delta for %q: %w", msg.Path, err)
	}
	return msg.Path, ops, nil
}
