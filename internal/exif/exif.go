// Package exif extracts capture metadata from the files an EOS RP produces,
// so imports can be filed under the date the shot was taken rather than the
// date it happened to be copied.
//
// It reads just enough of each container to find the timestamp:
//
//	JPEG  APP1/Exif -> TIFF IFD0 -> Exif IFD
//	CR3   ISO-BMFF moov -> Canon uuid box -> CMT1/CMT2 (TIFF blocks)
//	MP4   ISO-BMFF moov -> mvhd creation time
//
// There are no third-party dependencies, so the package can be bound into the
// mobile companion apps alongside the rest of the sync core.
package exif

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ErrNoMetadata means the file parsed correctly but carries no timestamp.
var ErrNoMetadata = errors.New("exif: no capture metadata found")

// Metadata is the subset of capture information rpsync uses.
type Metadata struct {
	// CapturedAt is the time the shot was taken, in the camera's local time
	// unless the file records a UTC offset.
	CapturedAt time.Time
	Make       string
	Model      string
}

// maxScan bounds how much of a file is examined, so a corrupt or hostile file
// cannot make the importer read gigabytes looking for a header.
const maxScan = 32 << 20

// FromFile reads capture metadata from a file on disk.
func FromFile(path string) (Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return Metadata{}, err
	}
	defer f.Close()
	return Read(f)
}

// Read reads capture metadata from r, which must be seekable.
func Read(r io.ReadSeeker) (Metadata, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Metadata{}, err
	}
	header := make([]byte, 12)
	n, err := io.ReadFull(r, header)
	if err != nil && n < 12 {
		return Metadata{}, ErrNoMetadata
	}

	switch {
	case header[0] == 0xFF && header[1] == 0xD8:
		return readJPEG(r)
	case string(header[4:8]) == "ftyp":
		return readBMFF(r, string(header[8:12]))
	default:
		return Metadata{}, ErrNoMetadata
	}
}

// ---------------------------------------------------------------- JPEG

// readJPEG walks JPEG markers looking for the APP1 Exif segment.
func readJPEG(r io.ReadSeeker) (Metadata, error) {
	if _, err := r.Seek(2, io.SeekStart); err != nil { // past SOI
		return Metadata{}, err
	}

	var head [4]byte
	for {
		if _, err := io.ReadFull(r, head[:2]); err != nil {
			return Metadata{}, ErrNoMetadata
		}
		if head[0] != 0xFF {
			return Metadata{}, ErrNoMetadata
		}
		marker := head[1]
		switch marker {
		case 0xD8, 0x01: // no payload
			continue
		case 0xD9, 0xDA: // end of image / start of scan: metadata is behind us
			return Metadata{}, ErrNoMetadata
		}
		if marker >= 0xD0 && marker <= 0xD7 { // restart markers
			continue
		}

		if _, err := io.ReadFull(r, head[2:4]); err != nil {
			return Metadata{}, ErrNoMetadata
		}
		size := int(binary.BigEndian.Uint16(head[2:4]))
		if size < 2 {
			return Metadata{}, ErrNoMetadata
		}
		payloadLen := size - 2

		if marker == 0xE1 && payloadLen > 6 {
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(r, payload); err != nil {
				return Metadata{}, ErrNoMetadata
			}
			if string(payload[:6]) == "Exif\x00\x00" {
				var md Metadata
				parseTIFF(payload[6:], &md, 0)
				return finish(md)
			}
			continue
		}
		if _, err := r.Seek(int64(payloadLen), io.SeekCurrent); err != nil {
			return Metadata{}, ErrNoMetadata
		}
	}
}

// ---------------------------------------------------------------- ISO-BMFF

// canonUUID identifies the Canon-private box inside moov that holds the CMT
// TIFF blocks in a CR3.
var canonUUID = [16]byte{
	0x85, 0xc0, 0xb6, 0x87, 0x82, 0x0f, 0x11, 0xe0,
	0x81, 0x11, 0xf4, 0xce, 0x46, 0x2b, 0x6a, 0x48,
}

func readBMFF(r io.ReadSeeker, brand string) (Metadata, error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return Metadata{}, err
	}
	if size > maxScan {
		size = maxScan
	}

	var md Metadata
	if err := walkBoxes(r, 0, size, 0, &md); err != nil && !errors.Is(err, io.EOF) {
		return Metadata{}, err
	}
	if md.CapturedAt.IsZero() {
		return Metadata{}, fmt.Errorf("%w (brand %q)", ErrNoMetadata, strings.TrimSpace(brand))
	}
	return finish(md)
}

// walkBoxes recurses through the container boxes that can hold timestamps.
func walkBoxes(r io.ReadSeeker, start, end int64, depth int, md *Metadata) error {
	if depth > 6 {
		return nil
	}
	pos := start
	for pos+8 <= end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		boxType := string(hdr[4:8])
		headerLen := int64(8)

		switch {
		case boxSize == 1: // 64-bit size
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return err
			}
			boxSize = int64(binary.BigEndian.Uint64(ext[:]))
			headerLen = 16
		case boxSize == 0: // extends to end of file
			boxSize = end - pos
		}
		if boxSize < headerLen || pos+boxSize > end {
			return nil // truncated or bogus: stop rather than guess
		}

		bodyStart, bodyEnd := pos+headerLen, pos+boxSize
		switch boxType {
		case "moov", "trak", "mdia":
			if err := walkBoxes(r, bodyStart, bodyEnd, depth+1, md); err != nil {
				return err
			}
		case "uuid":
			var id [16]byte
			if _, err := io.ReadFull(r, id[:]); err == nil && id == canonUUID {
				if err := readCanonCMT(r, bodyStart+16, bodyEnd, md); err != nil {
					return err
				}
			}
		case "mvhd":
			if t, ok := parseMVHD(r, bodyStart, bodyEnd); ok && md.CapturedAt.IsZero() {
				md.CapturedAt = t
			}
		}
		pos = bodyEnd
	}
	return nil
}

// readCanonCMT reads the TIFF blocks inside the Canon uuid box. CMT2 is the
// Exif IFD and holds DateTimeOriginal; CMT1 is IFD0 and holds the (less
// precise) file DateTime plus the camera make and model. CMT2 is parsed first
// so the shutter time wins.
func readCanonCMT(r io.ReadSeeker, start, end int64, md *Metadata) error {
	blocks := map[string][]byte{}
	pos := start
	for pos+8 <= end {
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return err
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		boxType := string(hdr[4:8])
		if boxSize < 8 || pos+boxSize > end {
			break
		}
		if boxType == "CMT1" || boxType == "CMT2" {
			if buf, err := readRange(r, pos+8, pos+boxSize); err == nil {
				blocks[boxType] = buf
			}
		}
		pos += boxSize
	}

	for _, name := range []string{"CMT2", "CMT1"} {
		if buf, ok := blocks[name]; ok {
			parseTIFF(buf, md, 0)
		}
	}
	return nil
}

// parseMVHD reads the movie header's creation time, which counts seconds from
// 1904-01-01 UTC.
func parseMVHD(r io.ReadSeeker, start, end int64) (time.Time, bool) {
	buf, err := readRange(r, start, end)
	if err != nil || len(buf) < 20 {
		return time.Time{}, false
	}
	version := buf[0]
	var secs uint64
	switch version {
	case 0:
		secs = uint64(binary.BigEndian.Uint32(buf[4:8]))
	case 1:
		if len(buf) < 28 {
			return time.Time{}, false
		}
		secs = binary.BigEndian.Uint64(buf[4:12])
	default:
		return time.Time{}, false
	}
	if secs == 0 {
		return time.Time{}, false
	}
	epoch1904 := time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	t := epoch1904.Add(time.Duration(secs) * time.Second)
	if t.Year() < 1970 || t.Year() > 2200 {
		return time.Time{}, false
	}
	return t, true
}

func readRange(r io.ReadSeeker, start, end int64) ([]byte, error) {
	n := end - start
	if n <= 0 || n > maxScan {
		return nil, errors.New("exif: box out of range")
	}
	if _, err := r.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ---------------------------------------------------------------- TIFF/IFD

const (
	tagDateTime         = 0x0132
	tagMake             = 0x010F
	tagModel            = 0x0110
	tagExifIFD          = 0x8769
	tagDateTimeOriginal = 0x9003
	tagDateTimeDigitize = 0x9004
	tagOffsetTimeOrig   = 0x9011
	tagSubSecTimeOrig   = 0x9291
)

// tiffState holds the strings gathered while walking a TIFF block, so the
// preferred timestamp can be chosen once everything has been seen.
type tiffState struct {
	dateTimeOriginal string
	dateTimeDigitize string
	dateTime         string
	offset           string
	subSec           string
}

// parseTIFF reads a TIFF block (as found in a JPEG APP1 payload or a CR3 CMT
// box) and merges what it finds into md. Existing values in md are not
// overwritten, so CMT2's capture time wins over CMT1's file time.
func parseTIFF(b []byte, md *Metadata, depth int) {
	if len(b) < 8 {
		return
	}
	var bo binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return
	}
	if bo.Uint16(b[2:4]) != 42 {
		return
	}
	var st tiffState
	walkIFD(b, bo, int64(bo.Uint32(b[4:8])), md, &st, depth)
	applyState(md, st)
}

func walkIFD(b []byte, bo binary.ByteOrder, offset int64, md *Metadata, st *tiffState, depth int) {
	if depth > 4 || offset <= 0 || offset+2 > int64(len(b)) {
		return
	}
	count := int64(bo.Uint16(b[offset : offset+2]))
	entries := offset + 2
	if entries+count*12 > int64(len(b)) {
		return
	}

	for i := int64(0); i < count; i++ {
		e := b[entries+i*12 : entries+i*12+12]
		tag := bo.Uint16(e[0:2])
		typ := bo.Uint16(e[2:4])
		n := int64(bo.Uint32(e[4:8]))

		switch tag {
		case tagExifIFD:
			if typ == 4 && n >= 1 {
				walkIFD(b, bo, int64(bo.Uint32(e[8:12])), md, st, depth+1)
			}
		case tagDateTimeOriginal:
			st.dateTimeOriginal = asciiValue(b, bo, e, typ, n)
		case tagDateTimeDigitize:
			st.dateTimeDigitize = asciiValue(b, bo, e, typ, n)
		case tagDateTime:
			st.dateTime = asciiValue(b, bo, e, typ, n)
		case tagOffsetTimeOrig:
			st.offset = asciiValue(b, bo, e, typ, n)
		case tagSubSecTimeOrig:
			st.subSec = asciiValue(b, bo, e, typ, n)
		case tagMake:
			if md.Make == "" {
				md.Make = asciiValue(b, bo, e, typ, n)
			}
		case tagModel:
			if md.Model == "" {
				md.Model = asciiValue(b, bo, e, typ, n)
			}
		}
	}
}

// asciiValue reads an ASCII tag, following the value offset when the payload
// does not fit in the entry's four inline bytes.
func asciiValue(b []byte, bo binary.ByteOrder, entry []byte, typ uint16, n int64) string {
	if typ != 2 || n <= 0 || n > int64(len(b)) {
		return ""
	}
	var raw []byte
	if n <= 4 {
		raw = entry[8 : 8+n]
	} else {
		off := int64(bo.Uint32(entry[8:12]))
		if off < 0 || off+n > int64(len(b)) {
			return ""
		}
		raw = b[off : off+n]
	}
	return strings.TrimSpace(strings.TrimRight(string(raw), "\x00"))
}

// applyState converts the collected strings into md.CapturedAt, preferring the
// moment the shutter fired over any later timestamp.
func applyState(md *Metadata, st tiffState) {
	if !md.CapturedAt.IsZero() {
		return
	}
	for _, candidate := range []string{st.dateTimeOriginal, st.dateTimeDigitize, st.dateTime} {
		if t, ok := parseEXIFTime(candidate, st.offset, st.subSec); ok {
			md.CapturedAt = t
			return
		}
	}
}

// parseEXIFTime parses "2024:05:04 10:30:00" plus optional "+09:00" offset and
// sub-second digits.
func parseEXIFTime(value, offset, subSec string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "0000") {
		return time.Time{}, false
	}
	loc := time.Local
	if off := strings.TrimSpace(offset); off != "" {
		if z, err := time.Parse("-07:00", off); err == nil {
			loc = z.Location()
		}
	}
	t, err := time.ParseInLocation("2006:01:02 15:04:05", value, loc)
	if err != nil {
		return time.Time{}, false
	}
	if t.Year() < 1970 {
		return time.Time{}, false
	}
	if ss := strings.TrimSpace(subSec); ss != "" {
		if frac, err := time.ParseDuration("0." + ss + "s"); err == nil {
			t = t.Add(frac)
		}
	}
	return t, true
}

func finish(md Metadata) (Metadata, error) {
	if md.CapturedAt.IsZero() {
		return md, ErrNoMetadata
	}
	return md, nil
}
