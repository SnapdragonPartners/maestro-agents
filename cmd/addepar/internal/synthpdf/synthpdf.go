// Package synthpdf writes small text-only PDFs for the intake agent's tests:
// synthetic statements that reproduce the real samples' layouts and traps
// without their confidential contents. The real samples never enter the
// repository, which is public (ADR 0015).
//
// Each page is lines of monospaced text (Courier, a standard font, so
// nothing is embedded), so pdftotext's layout mode reads back the columns as
// written. A PDF may be encrypted with copying forbidden, using the PDF
// standard security handler at revision 2 (40-bit RC4, empty user password),
// to test the copy-restriction path.
package synthpdf

import (
	"bytes"
	"crypto/md5" //nolint:gosec // the PDF standard security handler, revision 2, is defined over MD5 and RC4
	"crypto/rc4" //nolint:gosec // see above: reproducing a restricted PDF, not protecting anything
	"fmt"
	"strings"
)

// Options configures Build.
type Options struct {
	// FileID is the trailer's document ID; it seeds the encryption key.
	// Default "synthpdf".
	FileID string
	// CopyRestricted encrypts the PDF with printing allowed and copying,
	// changing and annotating forbidden, as the copy-restricted samples are.
	CopyRestricted bool
}

const (
	fontSize   = 8.0
	charWidth  = fontSize * 0.6 // Courier's advance is 600/1000 em
	lineHeight = 10.0
	margin     = 36.0
)

// Build returns a PDF with one page per element of pages, each page's text
// split into lines on '\n'. Text must be printable ASCII.
func Build(pages []string, opts Options) ([]byte, error) {
	if opts.FileID == "" {
		opts.FileID = "synthpdf"
	}
	for i, p := range pages {
		for _, r := range p {
			if r != '\n' && (r < ' ' || r > '~') {
				return nil, fmt.Errorf("synthpdf: page %d: non-ASCII or control character %q", i+1, r)
			}
		}
	}
	w := &writer{}
	n := len(pages)
	// Objects: 1 catalog, 2 pages, 3 font, then a page and its content per
	// page, then the encryption dictionary.
	encObj := 4 + 2*n
	var enc *security
	if opts.CopyRestricted {
		enc = newSecurity(opts.FileID)
	}

	w.header()
	w.object(1, "<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, n)
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	w.object(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	w.object(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Courier /Encoding /WinAnsiEncoding >>")
	for i, p := range pages {
		lines := strings.Split(p, "\n")
		cols := 0
		for _, l := range lines {
			cols = max(cols, len(l))
		}
		width := 2*margin + float64(cols)*charWidth
		height := 2*margin + float64(len(lines))*lineHeight
		pageObj, contentObj := 4+2*i, 5+2*i
		w.object(pageObj, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.1f %.1f] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>",
			width, height, contentObj))
		content := contentStream(lines, height)
		if enc != nil {
			content = enc.encrypt(contentObj, content)
		}
		w.stream(contentObj, content)
	}
	trailer := fmt.Sprintf("/Size %d /Root 1 0 R /ID [<%x> <%x>]", encObj, []byte(opts.FileID), []byte(opts.FileID))
	if enc != nil {
		w.object(encObj, enc.dict())
		trailer = fmt.Sprintf("/Size %d /Root 1 0 R /Encrypt %d 0 R /ID [<%x> <%x>]", encObj+1, encObj, []byte(opts.FileID), []byte(opts.FileID))
	}
	return w.finish(trailer), nil
}

func contentStream(lines []string, height float64) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "BT /F1 %.0f Tf %.0f TL %.1f %.1f Td\n", fontSize, lineHeight, margin, height-margin-fontSize)
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			fmt.Fprintf(&b, "(%s) Tj\n", escape(l))
		}
		b.WriteString("T*\n")
	}
	b.WriteString("ET\n")
	return b.Bytes()
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(s)
}

type writer struct {
	offsets map[int]int
	buf     bytes.Buffer
}

func (w *writer) header() {
	w.offsets = map[int]int{}
	w.buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
}

func (w *writer) object(n int, body string) {
	w.offsets[n] = w.buf.Len()
	fmt.Fprintf(&w.buf, "%d 0 obj\n%s\nendobj\n", n, body)
}

func (w *writer) stream(n int, data []byte) {
	w.offsets[n] = w.buf.Len()
	fmt.Fprintf(&w.buf, "%d 0 obj\n<< /Length %d >>\nstream\n", n, len(data))
	w.buf.Write(data)
	w.buf.WriteString("\nendstream\nendobj\n")
}

func (w *writer) finish(trailer string) []byte {
	xref := w.buf.Len()
	size := len(w.offsets) + 1
	fmt.Fprintf(&w.buf, "xref\n0 %d\n0000000000 65535 f \n", size)
	for i := 1; i < size; i++ {
		fmt.Fprintf(&w.buf, "%010d 00000 n \n", w.offsets[i])
	}
	fmt.Fprintf(&w.buf, "trailer\n<< %s >>\nstartxref\n%d\n%%%%EOF\n", trailer, xref)
	return w.buf.Bytes()
}

// security is the PDF standard security handler at revision 2: a 40-bit
// RC4 key from an empty user password (PDF 1.7, 7.6.3, algorithms 2 to 4).
type security struct {
	o, u []byte
	key  []byte
	p    int32
}

// pad is the 32-byte password padding the standard defines.
const pad = "\x28\xbf\x4e\x5e\x4e\x75\x8a\x41\x64\x00\x4e\x56\xff\xfa\x01\x08" +
	"\x2e\x2e\x00\xb6\xd0\x68\x3e\x80\x2f\x0c\xa9\xfe\x64\x53\x69\x7a"

// permissions: print (bit 3) only. Bits 7-8 and 13-32 are reserved and set;
// copy (bit 5), modify (bit 4) and annotate (bit 6) are clear.
const permissions = int32(-64 | 4)

func newSecurity(fileID string) *security {
	s := &security{p: permissions}
	// Algorithm 3: O from the owner password (here the padding itself).
	ownerKey := md5.Sum([]byte(pad)) //nolint:gosec // see package imports
	s.o = rc4XOR(ownerKey[:5], []byte(pad))
	// Algorithm 2: the file key from the user password, O, P and the ID.
	h := md5.New() //nolint:gosec // see package imports
	h.Write([]byte(pad))
	h.Write(s.o)
	p := uint32(s.p) //nolint:gosec // the permissions word is written as its two's-complement bytes
	h.Write([]byte{byte(p), byte(p >> 8), byte(p >> 16), byte(p >> 24)})
	h.Write([]byte(fileID))
	s.key = h.Sum(nil)[:5]
	// Algorithm 4: U is the padding encrypted with the file key.
	s.u = rc4XOR(s.key, []byte(pad))
	return s
}

func (s *security) dict() string {
	return fmt.Sprintf("<< /Filter /Standard /V 1 /R 2 /O <%x> /U <%x> /P %d >>", s.o, s.u, s.p)
}

// encrypt encrypts an object's stream with the object's key (algorithm 1).
func (s *security) encrypt(obj int, data []byte) []byte {
	h := md5.New() //nolint:gosec // see package imports
	h.Write(s.key)
	h.Write([]byte{byte(obj), byte(obj >> 8), byte(obj >> 16), 0, 0})
	return rc4XOR(h.Sum(nil)[:len(s.key)+5], data)
}

func rc4XOR(key, data []byte) []byte {
	c, err := rc4.NewCipher(key) //nolint:gosec // see package imports
	if err != nil {
		panic(fmt.Sprintf("synthpdf: rc4 key: %v", err)) // key sizes here are fixed and valid
	}
	out := make([]byte, len(data))
	c.XORKeyStream(out, data)
	return out
}
