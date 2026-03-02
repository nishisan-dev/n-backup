// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package agent

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// ---------------------------------------------------------------------------
// teeWriter tests
// ---------------------------------------------------------------------------

// TestTeeWriter_WritesToBothDestinations verifica que teeWriter escreve
// nos dois writers na ordem correta.
func TestTeeWriter_WritesToBothDestinations(t *testing.T) {
	var a, b bytes.Buffer
	tw := &teeWriter{a: &a, b: &b}

	data := []byte("hello backup world")
	n, err := tw.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes written, got %d", len(data), n)
	}

	if !bytes.Equal(a.Bytes(), data) {
		t.Errorf("writer a: expected %q, got %q", data, a.Bytes())
	}
	if !bytes.Equal(b.Bytes(), data) {
		t.Errorf("writer b: expected %q, got %q", data, b.Bytes())
	}
}

// TestTeeWriter_MultipleWrites verifica múltiplos writes acumulam.
func TestTeeWriter_MultipleWrites(t *testing.T) {
	var a, b bytes.Buffer
	tw := &teeWriter{a: &a, b: &b}

	tw.Write([]byte("chunk1"))
	tw.Write([]byte("chunk2"))
	tw.Write([]byte("chunk3"))

	expected := "chunk1chunk2chunk3"
	if a.String() != expected {
		t.Errorf("writer a: expected %q, got %q", expected, a.String())
	}
	if b.String() != expected {
		t.Errorf("writer b: expected %q, got %q", expected, b.String())
	}
}

// errWriter retorna erro na N-ésima escrita.
type errWriter struct {
	failAfter int
	count     int
}

func (ew *errWriter) Write(p []byte) (int, error) {
	ew.count++
	if ew.count > ew.failAfter {
		return 0, errors.New("injected write error")
	}
	return len(p), nil
}

// TestTeeWriter_FirstWriterError verifica que erro no primeiro writer é propagado.
func TestTeeWriter_FirstWriterError(t *testing.T) {
	a := &errWriter{failAfter: 0} // falha na primeira escrita
	b := &bytes.Buffer{}
	tw := &teeWriter{a: a, b: b}

	_, err := tw.Write([]byte("data"))
	if err == nil {
		t.Fatal("expected error from first writer")
	}
}

// TestTeeWriter_SecondWriterError verifica que erro no segundo writer é propagado.
func TestTeeWriter_SecondWriterError(t *testing.T) {
	a := &bytes.Buffer{}
	b := &errWriter{failAfter: 0} // falha na primeira escrita
	tw := &teeWriter{a: a, b: b}

	_, err := tw.Write([]byte("data"))
	if err == nil {
		t.Fatal("expected error from second writer")
	}
}

// shortWriter sempre escreve menos que len(p).
type shortWriter struct{}

func (sw *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		return 1, nil // escrita parcial
	}
	return len(p), nil
}

// TestTeeWriter_ShortWrite verifica que io.ErrShortWrite é retornado
// quando o primeiro writer escreve menos bytes que o pedido.
func TestTeeWriter_ShortWrite(t *testing.T) {
	a := &shortWriter{}
	b := &bytes.Buffer{}
	tw := &teeWriter{a: a, b: b}

	_, err := tw.Write([]byte("data"))
	if err != io.ErrShortWrite {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
}

// TestTeeWriter_EmptyWrite verifica que escrita vazia funciona sem erros.
func TestTeeWriter_EmptyWrite(t *testing.T) {
	var a, b bytes.Buffer
	tw := &teeWriter{a: &a, b: &b}

	n, err := tw.Write([]byte{})
	if err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes written, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Constants tests
// ---------------------------------------------------------------------------

// TestBackupConstants verifica que as constantes possuem valores razoáveis.
func TestBackupConstants(t *testing.T) {
	if maxResumeAttempts < 1 {
		t.Errorf("maxResumeAttempts should be >= 1, got %d", maxResumeAttempts)
	}
	if resumeBackoff <= 0 {
		t.Errorf("resumeBackoff should be > 0, got %v", resumeBackoff)
	}
	if singleStreamACKPollInterval <= 0 {
		t.Errorf("singleStreamACKPollInterval should be > 0, got %v", singleStreamACKPollInterval)
	}
	if MaxBackupDuration <= 0 {
		t.Errorf("MaxBackupDuration should be > 0, got %v", MaxBackupDuration)
	}
}
