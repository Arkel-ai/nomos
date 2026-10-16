// Package templates carries the controlled document templates Nomos renders
// with pandoc (docs/PLAN.md §7.3).
//
// They are embedded rather than read from disk for two reasons: the API binary
// then reproduces a document without depending on the image's file layout, and
// the reference-doc hash recorded on an artifact row is a hash of exactly the
// bytes this build shipped.
//
// Slice 1 ships ONE controlled bordereau template. `--reference-doc` carries
// the firm's charte; it does not reproduce section breaks, floating objects,
// content controls or exotic headers, and "charte-perfect DOCX" is explicitly
// not an MVP goal.
package templates

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"sync"
)

// Template versions. They travel onto artifacts.template_version, so a
// document can be reproduced later: bump the constant whenever the markdown
// template or the reference doc changes.
const (
	BordereauVersion     = "bordereau_v1"
	DocumentVersion      = "document_v1"
	AgentDocumentVersion = "agent_document_v1"
	ReferenceName        = "reference-bordereau-v1.docx"
)

// BordereauMarkdown is the text/template that produces the pandoc markdown for
// a bordereau de communication de pièces.
//
//go:embed bordereau_v1.md.tmpl
var BordereauMarkdown string

// DocumentMarkdown is the text/template for a drafted act or note.
//
//go:embed document_v1.md.tmpl
var DocumentMarkdown string

// AgentDocumentMarkdown is the text/template for a document the agent wrote
// as a verified structured revision: blocks, chronology and bordereau tables,
// inline « À vérifier » warnings, cited sources and the verification appendix.
//
//go:embed agent_document_v1.md.tmpl
var AgentDocumentMarkdown string

// Reference is the --reference-doc: a DOCX carrying the Word styles the
// templates address by name (Titre1/2/3, Corps, Citation, Mention,
// PiedDePage). Replace it with a firm's charte, keeping the style ids.
//
//go:embed reference-bordereau-v1.docx
var Reference []byte

var (
	refOnce sync.Once
	refHash string
)

// ReferenceSHA256 is the hash recorded on every artifact rendered with this
// reference doc. Two documents with the same template version, renderer
// version and reference hash were produced by the same machinery.
func ReferenceSHA256() string {
	refOnce.Do(func() {
		sum := sha256.Sum256(Reference)
		refHash = hex.EncodeToString(sum[:])
	})
	return refHash
}
