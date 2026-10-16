package agent

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// buildDOCX writes a minimal, valid word-processing package: the generated
// pièces the runner adds (a pending document, an injection attempt). The
// nonce lands in the core properties only, so the text is pinned by the
// manifest while the bytes — and therefore the ingestion's SHA-256 duplicate
// check — differ per trial.
func buildDOCX(doc GeneratedDocument, nonce string) ([]byte, error) {
	var body strings.Builder
	para := func(text string, bold bool) {
		body.WriteString(`<w:p><w:r>`)
		if bold {
			body.WriteString(`<w:rPr><w:b/></w:rPr>`)
		}
		body.WriteString(`<w:t xml:space="preserve">`)
		_ = xml.EscapeText(&body, []byte(text))
		body.WriteString(`</w:t></w:r></w:p>`)
	}
	para(doc.Title, true)
	for _, p := range doc.Paragraphs {
		para(p, false)
	}

	files := []struct{ name, content string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/></Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/></Relationships>`},
		{"word/document.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body.String() + `</w:body></w:document>`},
		{"docProps/core.xml", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>%s</dc:title><dc:identifier>%s</dc:identifier></cp:coreProperties>`, xmlEscape(doc.Title), xmlEscape(nonce))},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(w, f.content); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// docxText returns the visible text of a DOCX, one line per paragraph — what
// the lawyer reads in the downloaded file, which is what the fidelity check
// compares with the verified revision.
func docxText(raw []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", fmt.Errorf("not a zip package: %w", err)
	}
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
		}
	}
	if doc == nil {
		return "", fmt.Errorf("no word/document.xml")
	}
	rc, err := doc.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, 64<<20))
	var out strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				out.WriteByte(' ')
			case "br":
				out.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				out.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				out.Write(t)
			}
		}
	}
	return out.String(), nil
}
