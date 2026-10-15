package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
)

const (
	ivSize  = 16
	macSize = 32
	chunk   = 1 << 20
)

var errBadMAC = errors.New("MAC doesn't match")

// verifyMAC checks the HMAC-SHA256 at the end of an `IV ‖ ciphertext ‖ HMAC` file of size bytes, over everything
// before it.
func verifyMAC(open func() (io.ReadCloser, error), size int64, key []byte) error {
	if size < ivSize+aes.BlockSize+macSize {
		return errBadMAC
	}
	f, err := open()
	if err != nil {
		return err
	}
	defer f.Close()
	mac := hmac.New(sha256.New, key)
	if _, err = io.CopyN(mac, f, size-macSize); err != nil {
		return err
	}
	expected := make([]byte, macSize)
	if _, err = io.ReadFull(f, expected); err != nil {
		return err
	}
	if !hmac.Equal(mac.Sum(nil), expected) {
		return errBadMAC
	}
	return nil
}

// cbcReader streams the PKCS7-unpadded plaintext of `IV ‖ AES-256-CBC ciphertext ‖ HMAC`. It doesn't check the
// HMAC (verifyMAC does, in a pass of its own).
type cbcReader struct {
	src  io.Reader // the ciphertext only
	mode cipher.BlockMode
	// Decrypted bytes not yet returned, holding back the last block until the end, for unpadding.
	out  []byte
	held []byte
	done bool
}

func newCBCReader(r io.Reader, size int64, key []byte) (*cbcReader, error) {
	iv := make([]byte, ivSize)
	if _, err := io.ReadFull(r, iv); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	n := size - ivSize - macSize
	if n <= 0 || n%aes.BlockSize != 0 {
		return nil, invalid("encrypted data has an invalid length")
	}
	return &cbcReader{src: io.LimitReader(r, n), mode: cipher.NewCBCDecrypter(block, iv)}, nil
}

func (c *cbcReader) Read(p []byte) (int, error) {
	for len(c.out) == 0 {
		if c.done {
			return 0, io.EOF
		}
		buf := make([]byte, chunk)
		n, err := io.ReadFull(c.src, buf)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, err
		}
		if n%aes.BlockSize != 0 {
			return 0, invalid("encrypted data ends mid-block")
		}
		buf = buf[:n]
		c.mode.CryptBlocks(buf, buf)
		data := append(c.held, buf...)
		if err == nil {
			// More may follow: hold back the last block.
			c.out, c.held = data[:len(data)-aes.BlockSize], bytes.Clone(data[len(data)-aes.BlockSize:])
			continue
		}
		c.done = true
		if len(data) == 0 {
			return 0, invalid("encrypted data is empty")
		}
		pad := int(data[len(data)-1])
		if pad == 0 || pad > aes.BlockSize || pad > len(data) {
			return 0, invalid("bad padding")
		}
		for _, b := range data[len(data)-pad:] {
			if int(b) != pad {
				return 0, invalid("bad padding")
			}
		}
		c.out, c.held = data[:len(data)-pad], nil
	}
	n := copy(p, c.out)
	c.out = c.out[n:]
	return n, nil
}

// cbcWriter writes `IV ‖ AES-256-CBC-PKCS7(plaintext) ‖ HMAC-SHA256(IV ‖ ciphertext)`, streaming.
type cbcWriter struct {
	dst     io.Writer
	mode    cipher.BlockMode
	mac     hash.Hash
	pending []byte
}

func newCBCWriter(dst io.Writer, aesKey, macKey []byte) (*cbcWriter, error) {
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, ivSize)
	if _, err = rand.Read(iv); err != nil {
		return nil, err
	}
	w := &cbcWriter{dst: dst, mode: cipher.NewCBCEncrypter(block, iv), mac: hmac.New(sha256.New, macKey)}
	return w, w.emit(iv)
}

func (w *cbcWriter) emit(b []byte) error {
	w.mac.Write(b)
	_, err := w.dst.Write(b)
	return err
}

func (w *cbcWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	full := len(w.pending) - len(w.pending)%aes.BlockSize
	if full >= chunk {
		ct := make([]byte, full)
		w.mode.CryptBlocks(ct, w.pending[:full])
		w.pending = append([]byte(nil), w.pending[full:]...)
		if err := w.emit(ct); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Close pads and encrypts the rest, then writes the MAC. It doesn't close dst.
func (w *cbcWriter) Close() error {
	pad := aes.BlockSize - len(w.pending)%aes.BlockSize
	w.pending = append(w.pending, bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(w.pending))
	w.mode.CryptBlocks(ct, w.pending)
	w.pending = nil
	if err := w.emit(ct); err != nil {
		return err
	}
	_, err := w.dst.Write(w.mac.Sum(nil))
	return err
}
