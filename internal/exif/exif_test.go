package exif_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/exif"
)

func TestJPEGDateTimeOriginal(t *testing.T) {
	jpeg := buildJPEG(t, tiffBlock(t, map[uint16]string{
		0x010F: "Canon",
		0x0110: "Canon EOS RP",
		0x0132: "2024:05:04 08:00:00", // file time in IFD0
	}, map[uint16]string{
		0x9003: "2024:05:04 10:30:15", // shutter time in the Exif IFD
		0x9011: "+09:00",
	}))

	md, err := exif.Read(bytes.NewReader(jpeg))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := time.Date(2024, 5, 4, 10, 30, 15, 0, time.FixedZone("", 9*3600))
	if !md.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %s, want %s", md.CapturedAt, want)
	}
	if md.Make != "Canon" || md.Model != "Canon EOS RP" {
		t.Errorf("Make/Model = %q/%q", md.Make, md.Model)
	}
}

func TestJPEGFallsBackToIFD0DateTime(t *testing.T) {
	jpeg := buildJPEG(t, tiffBlock(t, map[uint16]string{
		0x0132: "2023:11:02 17:45:00",
	}, nil))

	md, err := exif.Read(bytes.NewReader(jpeg))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := time.Date(2023, 11, 2, 17, 45, 0, 0, time.Local)
	if !md.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %s, want %s", md.CapturedAt, want)
	}
}

func TestJPEGWithoutExif(t *testing.T) {
	// SOI, a comment segment, then SOS: valid JPEG, no metadata.
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xD8})
	buf.Write([]byte{0xFF, 0xFE, 0x00, 0x04, 'h', 'i'})
	buf.Write([]byte{0xFF, 0xDA})

	if _, err := exif.Read(bytes.NewReader(buf.Bytes())); !errors.Is(err, exif.ErrNoMetadata) {
		t.Fatalf("err = %v, want ErrNoMetadata", err)
	}
}

func TestCR3PrefersCMT2CaptureTime(t *testing.T) {
	cmt1 := tiffBlock(t, map[uint16]string{
		0x010F: "Canon",
		0x0110: "Canon EOS RP",
		0x0132: "2024:05:04 08:00:00",
	}, nil)
	cmt2 := tiffBlock(t, map[uint16]string{
		0x9003: "2024:05:04 10:30:15",
	}, nil)

	cr3 := buildCR3(cmt1, cmt2)
	md, err := exif.Read(bytes.NewReader(cr3))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := time.Date(2024, 5, 4, 10, 30, 15, 0, time.Local)
	if !md.CapturedAt.Equal(want) {
		t.Errorf("CapturedAt = %s, want the CMT2 shutter time %s", md.CapturedAt, want)
	}
	if md.Model != "Canon EOS RP" {
		t.Errorf("Model = %q, want it read from CMT1", md.Model)
	}
}

func TestMP4CreationTime(t *testing.T) {
	created := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	secs := uint32(created.Sub(time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)) / time.Second)

	mp4 := buildMP4(secs)
	md, err := exif.Read(bytes.NewReader(mp4))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !md.CapturedAt.Equal(created) {
		t.Errorf("CapturedAt = %s, want %s", md.CapturedAt, created)
	}
}

func TestUnknownFormat(t *testing.T) {
	if _, err := exif.Read(bytes.NewReader([]byte("not an image at all"))); !errors.Is(err, exif.ErrNoMetadata) {
		t.Fatalf("err = %v, want ErrNoMetadata", err)
	}
}

func TestTruncatedInputIsNotFatal(t *testing.T) {
	jpeg := buildJPEG(t, tiffBlock(t, map[uint16]string{0x0132: "2024:05:04 08:00:00"}, nil))
	for _, cut := range []int{3, 10, 24, len(jpeg) / 2} {
		if cut >= len(jpeg) {
			continue
		}
		// Must not panic, whatever it returns.
		_, _ = exif.Read(bytes.NewReader(jpeg[:cut]))
	}
}

func TestFromFile(t *testing.T) {
	jpeg := buildJPEG(t, tiffBlock(t, map[uint16]string{0x0132: "2022:01:02 03:04:05"}, nil))
	path := filepath.Join(t.TempDir(), "IMG_0001.JPG")
	if err := os.WriteFile(path, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	md, err := exif.FromFile(path)
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	if md.CapturedAt.Year() != 2022 {
		t.Errorf("CapturedAt = %s", md.CapturedAt)
	}
}

// ---------------------------------------------------------------- builders

// tiffBlock builds a little-endian TIFF block with IFD0 entries and, when
// exifTags is non-empty, a nested Exif IFD.
func tiffBlock(t *testing.T, ifd0, exifTags map[uint16]string) []byte {
	t.Helper()

	// Layout: header(8) | IFD0 | ExifIFD | string heap
	type entry struct {
		tag uint16
		val string
	}
	order := func(m map[uint16]string) []entry {
		var es []entry
		for tag, v := range m {
			es = append(es, entry{tag, v})
		}
		// Deterministic order; TIFF wants ascending tags.
		for i := 0; i < len(es); i++ {
			for j := i + 1; j < len(es); j++ {
				if es[j].tag < es[i].tag {
					es[i], es[j] = es[j], es[i]
				}
			}
		}
		return es
	}

	ifd0Entries := order(ifd0)
	exifEntries := order(exifTags)

	ifd0Count := len(ifd0Entries)
	if len(exifEntries) > 0 {
		ifd0Count++ // the Exif IFD pointer
	}

	ifd0Off := 8
	ifd0Len := 2 + ifd0Count*12 + 4
	exifOff := ifd0Off + ifd0Len
	exifLen := 0
	if len(exifEntries) > 0 {
		exifLen = 2 + len(exifEntries)*12 + 4
	}
	heapOff := exifOff + exifLen

	var heap bytes.Buffer
	le := binary.LittleEndian

	writeIFD := func(buf *bytes.Buffer, entries []entry, extra func(*bytes.Buffer)) {
		count := uint16(len(entries))
		if extra != nil {
			count++
		}
		binary.Write(buf, le, count)
		for _, e := range entries {
			binary.Write(buf, le, e.tag)
			binary.Write(buf, le, uint16(2)) // ASCII
			val := e.val + "\x00"
			binary.Write(buf, le, uint32(len(val)))
			if len(val) <= 4 {
				padded := make([]byte, 4)
				copy(padded, val)
				buf.Write(padded)
			} else {
				binary.Write(buf, le, uint32(heapOff+heap.Len()))
				heap.WriteString(val)
			}
		}
		if extra != nil {
			extra(buf)
		}
		binary.Write(buf, le, uint32(0)) // no next IFD
	}

	var ifd0Buf bytes.Buffer
	var extra func(*bytes.Buffer)
	if len(exifEntries) > 0 {
		extra = func(buf *bytes.Buffer) {
			binary.Write(buf, le, uint16(0x8769))
			binary.Write(buf, le, uint16(4)) // LONG
			binary.Write(buf, le, uint32(1))
			binary.Write(buf, le, uint32(exifOff))
		}
	}
	writeIFD(&ifd0Buf, ifd0Entries, extra)

	var exifBuf bytes.Buffer
	if len(exifEntries) > 0 {
		writeIFD(&exifBuf, exifEntries, nil)
	}

	var out bytes.Buffer
	out.WriteString("II")
	binary.Write(&out, le, uint16(42))
	binary.Write(&out, le, uint32(ifd0Off))
	out.Write(ifd0Buf.Bytes())
	out.Write(exifBuf.Bytes())
	out.Write(heap.Bytes())

	if out.Len() < heapOff {
		t.Fatalf("builder produced %d bytes, expected at least %d", out.Len(), heapOff)
	}
	return out.Bytes()
}

func buildJPEG(t *testing.T, tiff []byte) []byte {
	t.Helper()
	payload := append([]byte("Exif\x00\x00"), tiff...)

	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xD8})             // SOI
	buf.Write([]byte{0xFF, 0xE0, 0x00, 0x04}) // an APP0 the parser must skip
	buf.Write([]byte{0x00, 0x00})
	buf.Write([]byte{0xFF, 0xE1})
	binary.Write(&buf, binary.BigEndian, uint16(len(payload)+2))
	buf.Write(payload)
	buf.Write([]byte{0xFF, 0xDA}) // SOS
	return buf.Bytes()
}

func box(typ string, body []byte) []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(len(body)+8))
	buf.WriteString(typ)
	buf.Write(body)
	return buf.Bytes()
}

func buildCR3(cmt1, cmt2 []byte) []byte {
	uuidBody := append([]byte{
		0x85, 0xc0, 0xb6, 0x87, 0x82, 0x0f, 0x11, 0xe0,
		0x81, 0x11, 0xf4, 0xce, 0x46, 0x2b, 0x6a, 0x48,
	}, box("CMT1", cmt1)...)
	uuidBody = append(uuidBody, box("CMT2", cmt2)...)

	var out bytes.Buffer
	out.Write(box("ftyp", []byte("crx isom")))
	out.Write(box("moov", box("uuid", uuidBody)))
	return out.Bytes()
}

func buildMP4(creationSecs uint32) []byte {
	var mvhd bytes.Buffer
	mvhd.Write([]byte{0, 0, 0, 0}) // version 0 + flags
	binary.Write(&mvhd, binary.BigEndian, creationSecs)
	binary.Write(&mvhd, binary.BigEndian, creationSecs) // modification
	binary.Write(&mvhd, binary.BigEndian, uint32(1000)) // timescale
	binary.Write(&mvhd, binary.BigEndian, uint32(5000)) // duration
	mvhd.Write(make([]byte, 80))

	var out bytes.Buffer
	out.Write(box("ftyp", []byte("mp42isom")))
	out.Write(box("moov", box("mvhd", mvhd.Bytes())))
	return out.Bytes()
}
