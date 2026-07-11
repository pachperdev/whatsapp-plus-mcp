package media

import (
	"encoding/binary"
	"testing"
)

// TestAnalyzeOggOpusTruncatedPage es la regresión del bounds-check: una página
// Ogg que declara un tamaño mayor que los bytes disponibles no debe paniquear.
func TestAnalyzeOggOpusTruncatedPage(t *testing.T) {
	// Página con numSegments=1 y segLen=255 → pageSize=283, pero solo damos 40 bytes.
	buf := make([]byte, 40)
	copy(buf[0:4], "OggS")
	binary.LittleEndian.PutUint32(buf[18:22], 0) // pageSeqNum=0 (entra al bloque OpusHead)
	buf[26] = 1                                  // numSegments=1
	buf[27] = 0xFF                               // segLen=255 → pageSize se dispara

	// No debe paniquear ni devolver error (la firma OggS es válida).
	dur, wf, err := AnalyzeOggOpus(buf)
	if err != nil {
		t.Fatalf("no debería fallar con OggS válido pero truncado: %v", err)
	}
	if dur < 1 {
		t.Errorf("duración debería ser al menos 1, got %d", dur)
	}
	if len(wf) != 64 {
		t.Errorf("waveform debería ser de 64 bytes, got %d", len(wf))
	}
}

func TestAnalyzeOggOpusNotOgg(t *testing.T) {
	if _, _, err := AnalyzeOggOpus([]byte("no soy ogg")); err == nil {
		t.Error("datos sin firma OggS deberían devolver error")
	}
	if _, _, err := AnalyzeOggOpus(nil); err == nil {
		t.Error("nil debería devolver error")
	}
}

func TestPlaceholderWaveformLength(t *testing.T) {
	if got := len(placeholderWaveform(5)); got != 64 {
		t.Errorf("waveform debería ser de 64 bytes, got %d", got)
	}
}
