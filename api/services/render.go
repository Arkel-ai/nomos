package services

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/Arkel-ai/nomos/api/templates"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var renderLog = u.NewLogger("services.render")

// Rendering — DOCX through pandoc, and only pandoc (docs/PLAN.md §7.3).
//
// `pandoc-cli` is in Alpine 3.20 community (220 MB installed), so it runs in
// the API image without changing the base, and CGO_ENABLED=0 is irrelevant to
// invoking a dynamically linked executable. There is deliberately NO pure-Go
// OOXML fallback: two renderers double the test matrix and will diverge. If a
// firm's template one day defeats pandoc, the answer is a narrow OOXML path
// for THAT template class — not a generic second renderer.
//
// Five rules hold this file together:
//
//  1. What is converted is a SERVER-OWNED structured document. The templates
//     live in api/templates and every interpolated value is escaped, so a
//     piece title or a drafted paragraph cannot carry markup, an image
//     reference or raw HTML into the converter.
//  2. pandoc is invoked through utils.RunTool with an ARGUMENT ARRAY, a
//     per-job temp directory, size caps and a timeout. Never a shell string
//     built from a filename.
//  3. `--sandbox` is on: pandoc may touch only the files named on the command
//     line, so even a defect in the escaper cannot make it read the filesystem.
//  4. The resulting OOXML package is VALIDATED before anything is published —
//     structure, XML well-formedness, and that every style the document uses
//     actually resolves in word/styles.xml.
//  5. The pandoc version, the reference-doc hash and the template version are
//     recorded with the artifact, so the document can be reproduced later.
//
// Rendering runs on the render pool, concurrency 1 per pod: pandoc is
// CPU-bound and must not starve request handling (§4.2).

const (
	// renderTimeout bounds one pandoc invocation. A bordereau converts in
	// well under a second; anything near this is pathological input.
	renderTimeout = 60 * time.Second
	// maxMarkdownBytes caps the generated markdown handed to pandoc.
	maxMarkdownBytes = 8 << 20 // 8 MiB
	// maxDocxBytes caps the produced package.
	maxDocxBytes = 32 << 20 // 32 MiB
	// maxOOXMLEntry / maxOOXMLTotal are the zip-bomb guards applied when the
	// package is validated.
	maxOOXMLEntry = 64 << 20  // 64 MiB per entry, decompressed
	maxOOXMLTotal = 256 << 20 // 256 MiB decompressed, whole package
)

// ErrRendererUnavailable means pandoc is not installed in this image. It is a
// deployment problem, not a bad document: the caller degrades to 503.
var ErrRendererUnavailable = errors.New("pandoc not available")

// ErrInvalidPackage means the produced file is not a usable OOXML package. It
// is never published.
var ErrInvalidPackage = errors.New("invalid OOXML package")

// DraftNotice is the stamp an export the lawyer has not approved carries
// (§13). It is a Word-styled banner on the first page, not a footer nobody
// reads.
const DraftNotice = "projet — vérification de l'avocat requise"

// Attribution travels onto every rendered document: Licence Ouverte 2.0
// permits commercial reuse WITH attribution, and the licence does not erase
// personal-data obligations (§13).
const Attribution = "Document préparé avec Nomos — sources ouvertes (Légifrance, Judilibre — Licence Ouverte 2.0). " +
	"Vérification et signature par l'avocat requises avant tout usage."

// ---------------------------------------------------------------------------
// The server-owned documents
// ---------------------------------------------------------------------------

// BordereauPiece is one row of the bordereau. Every field is already a
// display string: the renumbering and the renaming happened in export.go, from
// the piece metadata the lawyer approved.
type BordereauPiece struct {
	Ordinal  int
	Title    string
	Date     string
	Kind     string
	FileName string
}

// BordereauDoc is the structured bordereau. It is built by the server from
// approved piece rows — never from model output.
type BordereauDoc struct {
	DossierTitle    string
	ClientReference string
	MatterType      string
	GeneratedOn     string
	PieceCount      int
	Pieces          []BordereauPiece
	// Footnotes carry the observations the export must surface: a drifted
	// citation, an unapproved piece. Shapes and statements, never content.
	Footnotes []string
	// DraftNotice is empty for an approved export and DraftNotice otherwise.
	DraftNotice string
	Attribution string
}

// RenderResult is one converted document plus everything needed to reproduce
// it.
type RenderResult struct {
	Bytes           []byte
	CharCount       int
	TemplateVersion string
	// RendererVersion is the pandoc version string ("pandoc 3.8.1").
	RendererVersion string
	ReferenceSHA256 string
	SHA256          string
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// RenderBordereau converts a bordereau into DOCX.
func RenderBordereau(ctx context.Context, doc BordereauDoc) (RenderResult, error) {
	if doc.Attribution == "" {
		doc.Attribution = Attribution
	}
	if doc.PieceCount == 0 {
		doc.PieceCount = len(doc.Pieces)
	}
	escaped := doc
	escaped.DossierTitle = escapeMarkdown(doc.DossierTitle)
	escaped.ClientReference = escapeMarkdown(doc.ClientReference)
	escaped.MatterType = escapeMarkdown(doc.MatterType)
	escaped.GeneratedOn = escapeMarkdown(doc.GeneratedOn)
	escaped.DraftNotice = escapeMarkdown(doc.DraftNotice)
	escaped.Attribution = escapeMarkdown(doc.Attribution)
	escaped.Pieces = make([]BordereauPiece, 0, len(doc.Pieces))
	chars := 0
	for _, p := range doc.Pieces {
		escaped.Pieces = append(escaped.Pieces, BordereauPiece{
			Ordinal:  p.Ordinal,
			Title:    escapeTableCell(p.Title),
			Date:     escapeTableCell(p.Date),
			Kind:     escapeTableCell(p.Kind),
			FileName: escapeTableCell(p.FileName),
		})
		chars += len([]rune(p.Title)) + len([]rune(p.FileName))
	}
	escaped.Footnotes = escapeAll(doc.Footnotes)

	md, err := executeTemplate(templates.BordereauVersion, templates.BordereauMarkdown, escaped)
	if err != nil {
		return RenderResult{}, err
	}
	return renderMarkdown(ctx, md, templates.BordereauVersion, chars)
}

// ---------------------------------------------------------------------------
// Agent documents — rendered only from a verified revision
// ---------------------------------------------------------------------------

// ErrArtifactFidelity means a rendered DOCX does not carry the verified
// revision's text or one of its warnings. It is never staged.
var ErrArtifactFidelity = errors.New("rendered document does not match the verified revision")

// agentDocView is the server-owned shape behind agent_document_v1. Every
// string in it is already escaped; Table is a pipe table built from escaped
// cells.
type agentDocView struct {
	DraftNotice string
	Title       string
	Notices     []string
	Sections    []agentDocSection
	Sources     []string
	Appendix    []string
	Attribution string
}

type agentDocSection struct {
	Style string
	Text  string
	Table string
}

// AgentDocumentRender is a rendered agent document plus what its fidelity
// check and its artifact row need.
type AgentDocumentRender struct {
	RenderResult
	// Expected are the texts the package must contain: the title, every
	// block, every warning and notice.
	Expected      []string
	PieceWarnings int
	// Partial: an exhaustive request whose coverage is incomplete. The
	// document says so on its face.
	Partial bool
}

var agentDocTitles = map[string]string{
	types.AgentOutputAnswer:     "Réponse",
	types.AgentOutputChronology: "Chronologie",
	types.AgentOutputSynthesis:  "Note de synthèse",
	types.AgentOutputLegalNote:  "Note juridique",
	types.AgentOutputActe:       "Acte",
	types.AgentOutputBordereau:  "Bordereau de communication de pièces",
}

// RenderVerifiedDocument converts a verified agent revision into DOCX through
// the one pandoc path. It accepts nothing but a revision paired with its
// matching, download-allowing verification, and it never changes a word of
// the revision: pièce mismatches are rendered as « À vérifier » warnings
// beside the content they concern, never corrected. sources maps every
// evidence span the revision cites onto its display label.
func RenderVerifiedDocument(ctx context.Context, rev VerifiedRevision, sources map[string]string) (AgentDocumentRender, error) {
	if err := checkVerifiedPair(rev); err != nil {
		return AgentDocumentRender{}, err
	}
	var body types.DocumentRevisionBody
	if err := json.Unmarshal(rev.Revision.Body, &body); err != nil {
		return AgentDocumentRender{}, fmt.Errorf("%w: unreadable revision body", ErrNotVerified)
	}
	var report types.VerificationReport
	if err := json.Unmarshal(rev.Verification.Report, &report); err != nil {
		return AgentDocumentRender{}, fmt.Errorf("%w: unreadable verification report", ErrNotVerified)
	}

	out := AgentDocumentRender{}
	title := collapseWhitespace(firstNonEmpty(rev.Revision.Title, agentDocTitles[rev.Revision.Kind], "Document"))
	out.Expected = append(out.Expected, title)

	numbers := map[string]int{}
	order := revisionEvidenceOrder(body)
	for i, id := range order {
		numbers[id] = i + 1
	}
	markers := func(b types.DocumentBlock) string {
		var ns []int
		seen := map[int]bool{}
		for _, c := range b.Claims {
			for _, id := range claimEvidenceIDs(c) {
				if n := numbers[id]; n > 0 && !seen[n] {
					seen[n] = true
					ns = append(ns, n)
				}
			}
		}
		sort.Ints(ns)
		var s strings.Builder
		for _, n := range ns {
			fmt.Fprintf(&s, " [%d]", n)
		}
		return s.String()
	}

	view := agentDocView{
		DraftNotice: escapeMarkdown(DraftNotice),
		Title:       escapeMarkdown(title),
		Attribution: escapeMarkdown(Attribution),
	}

	blockWarnings := map[string][]string{}
	seen := map[string]bool{}
	var warnings []string
	for _, iss := range report.Issues {
		if iss.Severity != types.SeverityWarning || iss.Correction == "" {
			continue
		}
		if iss.Check == types.CheckPieceClaims {
			out.PieceWarnings++
		}
		key := iss.BlockID + "|" + iss.Correction
		if seen[key] {
			continue
		}
		seen[key] = true
		warnings = append(warnings, iss.Correction)
		out.Expected = append(out.Expected, iss.Correction)
		if iss.BlockID == "" {
			view.Notices = append(view.Notices, escapeParagraph(iss.Correction))
			continue
		}
		blockWarnings[iss.BlockID] = append(blockWarnings[iss.BlockID], iss.Correction)
	}
	if cov := report.Coverage; cov != nil {
		out.Partial = cov.Exhaustive && !cov.Complete
		if cov.Complete {
			notice := fmt.Sprintf("Périmètre : %d pièce(s), toutes lues intégralement (registre de lecture contrôlé).", cov.TotalPieces)
			view.Notices = append(view.Notices, escapeParagraph(notice))
			out.Expected = append(out.Expected, notice)
		}
	}

	pieceCell := func(b types.DocumentBlock) string {
		var labels []string
		done := map[string]bool{}
		for _, c := range b.Claims {
			for _, id := range claimEvidenceIDs(c) {
				if l := sources[id]; l != "" && !done[id] {
					done[id] = true
					labels = append(labels, l)
				}
			}
		}
		if len(labels) == 0 {
			return "—"
		}
		return strings.Join(labels, " ; ")
	}

	chars := 0
	blocks := body.Blocks
	for i := 0; i < len(blocks); {
		b := blocks[i]
		out.Expected = append(out.Expected, b.Text)
		chars += len([]rune(b.Text))
		switch b.Role {
		case types.BlockChronologyRow, types.BlockBordereauRow:
			role := b.Role
			var rows [][]string
			j := i
			for ; j < len(blocks) && blocks[j].Role == role; j++ {
				r := blocks[j]
				if j > i {
					out.Expected = append(out.Expected, r.Text)
					chars += len([]rune(r.Text))
				}
				text := r.Text
				if ws := blockWarnings[r.ID]; len(ws) > 0 {
					text += " — " + strings.Join(ws, " — ")
				}
				if role == types.BlockChronologyRow {
					date := "date non établie"
					if r.Chronology != nil {
						date = FrenchPartialDate(r.Chronology.Date.PartialDate) + dateRoleSuffix(r.Chronology.Date.Role)
					}
					rows = append(rows, []string{date, text, pieceCell(r)})
				} else {
					ordinal, date := "", "sans date"
					if r.Bordereau != nil {
						ordinal = fmt.Sprintf("%d", r.Bordereau.Ordinal)
						if r.Bordereau.Date != nil {
							date = FrenchPartialDate(*r.Bordereau.Date)
						}
					}
					rows = append(rows, []string{ordinal, text, date})
				}
			}
			if role == types.BlockChronologyRow {
				view.Sections = append(view.Sections, agentDocSection{Table: pipeTable(
					[]string{"Date", "Événement", "Source"}, "|:-----|:---------|:-------|", rows)})
			} else {
				view.Sections = append(view.Sections, agentDocSection{Table: pipeTable(
					[]string{"N°", "Pièce", "Date"}, "|---:|:------|:-----|", rows)})
			}
			i = j
			continue
		}

		style, text := "Corps", b.Text+markers(b)
		switch b.Role {
		case types.BlockHeading:
			style = "Titre2"
			if b.Level > 1 {
				style = "Titre3"
			}
		case types.BlockListItem:
			text = "– " + text
		case types.BlockLimitation:
			style, text = "Mention", "Limite : "+text
		case types.BlockWarning:
			style = "Mention"
		}
		view.Sections = append(view.Sections, agentDocSection{Style: style, Text: escapeParagraph(text)})
		for _, w := range blockWarnings[b.ID] {
			view.Sections = append(view.Sections, agentDocSection{Style: "Mention", Text: escapeParagraph(w)})
		}
		i++
	}

	for _, id := range order {
		label := firstNonEmpty(sources[id], "source sans libellé")
		view.Sources = append(view.Sources, escapeMarkdown(collapseWhitespace(fmt.Sprintf("[%d] %s", numbers[id], label))))
	}
	view.Appendix = escapeAll(verificationAppendix(report, warnings, rev.Verification.VerifierVersion, sources, order))

	md, err := executeTemplate(templates.AgentDocumentVersion, templates.AgentDocumentMarkdown, view)
	if err != nil {
		return AgentDocumentRender{}, err
	}
	res, err := renderMarkdown(ctx, md, templates.AgentDocumentVersion, chars)
	if err != nil {
		return AgentDocumentRender{}, err
	}
	out.RenderResult = res
	return out, nil
}

func dateRoleSuffix(role string) string {
	switch role {
	case types.DateRoleDocument:
		return " (date du document)"
	case types.DateRoleReceipt:
		return " (date de réception)"
	}
	return ""
}

// pipeTable builds a pandoc pipe table from raw cells, escaping each one.
func pipeTable(header []string, align string, rows [][]string) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(header, " | ") + " |\n")
	b.WriteString(align + "\n")
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = escapeTableCell(c)
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	return b.String()
}

// verificationAppendix is the server-written account of the automatic check:
// what was checked, every warning, what was NOT checked. Raw text, escaped by
// the caller.
func verificationAppendix(r types.VerificationReport, warnings []string, version string, sources map[string]string, order []string) []string {
	out := []string{fmt.Sprintf("Contrôle automatique du %s (%s).", FrenchDate(time.Unix(r.CheckedAt, 0).UTC()), version)}
	if len(warnings) == 0 {
		out = append(out, "Aucune anomalie détectée par le contrôle automatique.")
	} else {
		out = append(out, fmt.Sprintf("%d point(s) à vérifier, signalé(s) dans le document par « À vérifier » :", len(warnings)))
		out = append(out, warnings...)
	}
	if len(order) > 0 {
		out = append(out, fmt.Sprintf("%d source(s) citée(s), chacune rattachée au passage exact retourné pendant la recherche.", len(order)))
	}
	for _, c := range r.Checks {
		if c.Check == types.CheckLegalCitations && c.Status == types.CheckPassed && c.Checked > 0 {
			out = append(out, "Références juridiques : contrôlées contre la version enregistrée dans Nomos (texte, version applicable, extrait cité). "+
				"Elles n'ont pas été recontrôlées auprès de la source officielle.")
		}
	}
	if cov := r.Coverage; cov != nil {
		out = append(out, fmt.Sprintf("Couverture de lecture : %d pièce(s) sur %d lues intégralement.", cov.FullyRead, cov.TotalPieces))
	}
	return append(out, "Ce contrôle compare les passages cités aux sources ; il ne remplace pas la relecture et la signature de l'avocat.")
}

// DocxText extracts the visible text of a package's word/document.xml, one
// line per paragraph; tabs and line breaks become spaces.
func DocxText(pkg []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return "", fmt.Errorf("%w: not a zip archive: %v", ErrInvalidPackage, err)
	}
	doc, err := readZipEntry(zr, "word/document.xml")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	dec := xml.NewDecoder(bytes.NewReader(doc))
	inText := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			return "", fmt.Errorf("%w: word/document.xml: %v", ErrInvalidPackage, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab", "br":
				b.WriteByte(' ')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
}

// CheckDocxFidelity compares a rendered package with the verified revision:
// every expected text — title, blocks, warnings — must appear in it, under
// the quote-matching normalisation.
func CheckDocxFidelity(pkg []byte, expected []string) error {
	text, err := DocxText(pkg)
	if err != nil {
		return err
	}
	norm := normalizeForQuoteMatch(text)
	for i, e := range expected {
		if !strings.Contains(norm, normalizeForQuoteMatch(e)) {
			return fmt.Errorf("%w: expected text %d of %d is missing", ErrArtifactFidelity, i+1, len(expected))
		}
	}
	return nil
}

// renderMarkdown is the whole pandoc path: stage the inputs in a private job
// directory, convert, validate, and report the provenance.
func renderMarkdown(ctx context.Context, md, templateVersion string, charCount int) (RenderResult, error) {
	if len(md) > maxMarkdownBytes {
		return RenderResult{}, fmt.Errorf("%w: source document too large (%d bytes)", ErrInvalidPackage, len(md))
	}

	dir, cleanup, err := u.JobDir("render")
	if err != nil {
		return RenderResult{}, err
	}
	defer cleanup()

	srcPath := filepath.Join(dir, "document.md")
	refPath := filepath.Join(dir, templates.ReferenceName)
	outPath := filepath.Join(dir, "document.docx")
	if err := os.WriteFile(srcPath, []byte(md), 0o600); err != nil {
		return RenderResult{}, err
	}
	if err := os.WriteFile(refPath, templates.Reference, 0o600); err != nil {
		return RenderResult{}, err
	}

	// An argument ARRAY, never a shell string. --sandbox restricts pandoc's
	// I/O to the files named right here.
	_, err = u.RunTool(ctx, u.ToolSpec{
		Name: "pandoc",
		Args: []string{
			"--sandbox",
			"--from=markdown",
			"--to=docx",
			"--reference-doc=" + refPath,
			"--output=" + outPath,
			srcPath,
		},
		Dir:           dir,
		InputPaths:    []string{srcPath, refPath},
		Timeout:       renderTimeout,
		MaxOutput:     1 << 20, // pandoc writes the docx to --output, not stdout
		MaxInputBytes: maxMarkdownBytes,
	})
	if err != nil {
		if u.IsToolMissing(err) {
			renderLog.Errorf("pandoc is not installed in this image — DOCX rendering is unavailable")
			return RenderResult{}, ErrRendererUnavailable
		}
		renderLog.Errorf("pandoc failed (template=%s): %v", templateVersion, err)
		return RenderResult{}, fmt.Errorf("conversion pandoc échouée: %w", err)
	}

	info, err := os.Stat(outPath)
	if err != nil {
		return RenderResult{}, fmt.Errorf("%w: pandoc produced no output", ErrInvalidPackage)
	}
	if info.Size() > maxDocxBytes {
		return RenderResult{}, fmt.Errorf("%w: produced document too large (%d bytes)", ErrInvalidPackage, info.Size())
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return RenderResult{}, err
	}
	if err := ValidateOOXML(out); err != nil {
		renderLog.Errorf("Refusing to publish an invalid OOXML package (template=%s): %v", templateVersion, err)
		return RenderResult{}, err
	}

	version, err := PandocVersion(ctx)
	if err != nil {
		return RenderResult{}, err
	}
	sum := sha256.Sum256(out)

	renderLog.Infof("Rendered DOCX (template=%s renderer=%q bytes=%d chars=%d reference=%s)",
		templateVersion, version, len(out), charCount, templates.ReferenceSHA256()[:12])

	return RenderResult{
		Bytes:           out,
		CharCount:       charCount,
		TemplateVersion: templateVersion,
		RendererVersion: version,
		ReferenceSHA256: templates.ReferenceSHA256(),
		SHA256:          hex.EncodeToString(sum[:]),
	}, nil
}

func executeTemplate(name, body string, data any) (string, error) {
	tpl, err := template.New(name).Parse(body)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	return buf.String(), nil
}

// ---------------------------------------------------------------------------
// Renderer version
// ---------------------------------------------------------------------------

var (
	pandocOnce    sync.Once
	pandocVersion string
	pandocErr     error
)

// PandocVersion returns the renderer version string recorded on every
// artifact. Resolved once per process: the binary cannot change under a
// running pod.
func PandocVersion(ctx context.Context) (string, error) {
	pandocOnce.Do(func() {
		out, err := u.RunTool(ctx, u.ToolSpec{
			Name:      "pandoc",
			Args:      []string{"--version"},
			Timeout:   10 * time.Second,
			MaxOutput: 64 << 10,
		})
		if err != nil {
			if u.IsToolMissing(err) {
				pandocErr = ErrRendererUnavailable
				return
			}
			pandocErr = err
			return
		}
		line, _, _ := strings.Cut(string(out), "\n")
		pandocVersion = strings.TrimSpace(line)
		if pandocVersion == "" {
			pandocVersion = "pandoc (version unknown)"
		}
	})
	return pandocVersion, pandocErr
}

// ---------------------------------------------------------------------------
// OOXML validation
// ---------------------------------------------------------------------------

// requiredOOXMLParts are the parts a Word package must carry for Word to open
// it at all.
var requiredOOXMLParts = []string{
	"[Content_Types].xml",
	"_rels/.rels",
	"word/document.xml",
	"word/styles.xml",
}

// ValidateOOXML checks a produced .docx before it is published.
//
// A file that pandoc exited 0 on is not automatically a document a court will
// accept: this checks that the package is a well-formed zip carrying the
// required parts, that document.xml and styles.xml parse as XML, and — the
// part that actually catches template regressions — that EVERY paragraph and
// character style the document references resolves to a style defined in
// styles.xml. A document referencing "Titre1" when the reference doc lost that
// style renders as unformatted body text, silently.
func ValidateOOXML(pkg []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return fmt.Errorf("%w: not a zip archive: %v", ErrInvalidPackage, err)
	}

	present := map[string]bool{}
	var total int64
	for _, f := range zr.File {
		name := f.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return fmt.Errorf("%w: unsafe entry name %q", ErrInvalidPackage, name)
		}
		if f.UncompressedSize64 > maxOOXMLEntry {
			return fmt.Errorf("%w: entry %q is too large", ErrInvalidPackage, name)
		}
		total += int64(f.UncompressedSize64)
		if total > maxOOXMLTotal {
			return fmt.Errorf("%w: package decompresses to more than %d bytes", ErrInvalidPackage, maxOOXMLTotal)
		}
		present[name] = true
	}
	for _, want := range requiredOOXMLParts {
		if !present[want] {
			return fmt.Errorf("%w: missing part %q", ErrInvalidPackage, want)
		}
	}

	document, err := readZipEntry(zr, "word/document.xml")
	if err != nil {
		return err
	}
	stylesXML, err := readZipEntry(zr, "word/styles.xml")
	if err != nil {
		return err
	}
	if err := wellFormedXML(document); err != nil {
		return fmt.Errorf("%w: word/document.xml: %v", ErrInvalidPackage, err)
	}
	if err := wellFormedXML(stylesXML); err != nil {
		return fmt.Errorf("%w: word/styles.xml: %v", ErrInvalidPackage, err)
	}

	defined, err := definedStyleIDs(stylesXML)
	if err != nil {
		return err
	}
	used, err := usedStyleIDs(document)
	if err != nil {
		return err
	}
	var unresolved []string
	for _, id := range used {
		if !defined[id] {
			unresolved = append(unresolved, id)
		}
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return fmt.Errorf("%w: styles used but not defined: %s",
			ErrInvalidPackage, strings.Join(unresolved, ", "))
	}
	return nil
}

func readZipEntry(zr *zip.Reader, name string) ([]byte, error) {
	f, err := zr.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open %q: %v", ErrInvalidPackage, name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxOOXMLEntry))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read %q: %v", ErrInvalidPackage, name, err)
	}
	return b, nil
}

func wellFormedXML(b []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// definedStyleIDs collects w:styleId from word/styles.xml.
func definedStyleIDs(b []byte) (map[string]bool, error) {
	out := map[string]bool{}
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: word/styles.xml: %v", ErrInvalidPackage, err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "style" {
			continue
		}
		for _, a := range start.Attr {
			if a.Name.Local == "styleId" {
				out[a.Value] = true
			}
		}
	}
}

// usedStyleIDs collects the w:val of every w:pStyle / w:rStyle / w:tblStyle in
// word/document.xml.
func usedStyleIDs(b []byte) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: word/document.xml: %v", ErrInvalidPackage, err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "pStyle", "rStyle", "tblStyle":
		default:
			continue
		}
		for _, a := range start.Attr {
			if a.Name.Local == "val" && a.Value != "" && !seen[a.Value] {
				seen[a.Value] = true
				out = append(out, a.Value)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Escaping
// ---------------------------------------------------------------------------

// mdPunctuation is every ASCII punctuation character pandoc's markdown reader
// accepts a backslash escape for. Escaping ALL of them is blunt and correct:
// the rendered output is identical, and no combination of characters in a
// piece title or a drafted paragraph can start a link, an image, a raw HTML
// block, a fenced div or a table cell boundary.
const mdPunctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// escapeMarkdown makes s inert for the pandoc markdown reader.
func escapeMarkdown(s string) string {
	var b strings.Builder
	b.Grow(len(s) * 2)
	for _, r := range s {
		if r < 128 && strings.ContainsRune(mdPunctuation, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escapeTableCell additionally flattens newlines: a pipe table row is one
// line, and a stray newline would silently truncate the table.
func escapeTableCell(s string) string {
	return escapeMarkdown(collapseWhitespace(s))
}

// escapeParagraph keeps single newlines (pandoc renders them as soft breaks
// inside a fenced div) but removes blank lines, which would close the div and
// let the rest of the block escape its style.
func escapeParagraph(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		kept = append(kept, escapeMarkdown(l))
	}
	return strings.Join(kept, "\\\n")
}

func escapeAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, escapeMarkdown(collapseWhitespace(s)))
	}
	return out
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// docxMime is the OOXML word-processing media type, written once here rather
// than at each call site.
const docxMime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
