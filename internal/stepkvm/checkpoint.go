// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package stepkvm

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// State is everything needed to resume a chunk chain: the last snapshot the
// chain returned, the client-held blobs and the stream assembled so far.
type State struct {
	Snapshot   []byte
	MemImg     []byte
	PathBlob   []byte
	Noise      []byte
	Stream     []byte
	Calls      int
	NoiseCalls int
	Retries    int
	Steps      uint64
	Budget     uint64
	Done       bool // the stream is complete
}

// RunKey identifies one run: the stepped interpreter, the program, the seed
// and the params. A checkpoint resumes only the run it was written for.
func RunKey(stepRunner string, program []byte, seed int32, params []int64) [32]byte {
	h := sha256.New()
	h.Write([]byte(stepRunner))
	h.Write(program)
	binary.Write(h, binary.BigEndian, seed)
	for _, p := range params {
		binary.Write(h, binary.BigEndian, p)
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

const ckptMagic = "OWNABLE-CKPT-2\n"

// SaveCheckpoint writes the state atomically (a temporary file, then rename).
func SaveCheckpoint(path string, key [32]byte, s *State) error {
	var b bytes.Buffer
	b.WriteString(ckptMagic)
	b.Write(key[:])
	done := uint64(0)
	if s.Done {
		done = 1
	}
	for _, v := range []uint64{uint64(s.Calls), uint64(s.NoiseCalls), uint64(s.Retries), s.Steps, s.Budget, done} {
		binary.Write(&b, binary.BigEndian, v)
	}
	for _, blob := range [][]byte{s.Snapshot, s.MemImg, s.PathBlob, s.Noise, s.Stream} {
		binary.Write(&b, binary.BigEndian, uint64(len(blob)))
		b.Write(blob)
	}
	sum := sha256.Sum256(b.Bytes())
	b.Write(sum[:])
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ownable-ckpt-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ErrNoCheckpoint means there is no checkpoint file yet.
var ErrNoCheckpoint = errors.New("no checkpoint")

// LoadCheckpoint reads a checkpoint written for the same run key.
func LoadCheckpoint(path string, key [32]byte) (*State, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCheckpoint
	}
	if err != nil {
		return nil, err
	}
	if len(raw) < len(ckptMagic)+32+32 || string(raw[:len(ckptMagic)]) != ckptMagic {
		return nil, fmt.Errorf("%s is not a checkpoint file", path)
	}
	body, sum := raw[:len(raw)-32], raw[len(raw)-32:]
	if s := sha256.Sum256(body); !bytes.Equal(s[:], sum) {
		return nil, fmt.Errorf("checkpoint %s is damaged", path)
	}
	r := bytes.NewReader(body[len(ckptMagic):])
	var k [32]byte
	io.ReadFull(r, k[:])
	if k != key {
		return nil, fmt.Errorf("checkpoint %s belongs to a different run (another token, program or interpreter)", path)
	}
	var nums [6]uint64
	if err := binary.Read(r, binary.BigEndian, &nums); err != nil {
		return nil, fmt.Errorf("checkpoint %s: %w", path, err)
	}
	s := &State{Calls: int(nums[0]), NoiseCalls: int(nums[1]), Retries: int(nums[2]), Steps: nums[3], Budget: nums[4], Done: nums[5] == 1}
	for _, dst := range []*[]byte{&s.Snapshot, &s.MemImg, &s.PathBlob, &s.Noise, &s.Stream} {
		var n uint64
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, fmt.Errorf("checkpoint %s: %w", path, err)
		}
		if n > uint64(r.Len()) {
			return nil, fmt.Errorf("checkpoint %s: bad section length", path)
		}
		*dst = make([]byte, n)
		io.ReadFull(r, *dst)
	}
	return s, nil
}
