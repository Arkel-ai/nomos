# api/templates

The controlled document templates (docs/PLAN.md §7.3). One renderer — pandoc —
and one bordereau template; there is deliberately no second, pure-Go OOXML path.

| File | What it is |
| ---- | ---------- |
| `bordereau_v1.md.tmpl` | `text/template` → pandoc markdown for the bordereau de communication de pièces. |
| `document_v1.md.tmpl` | `text/template` → pandoc markdown for a drafted act / note de synthèse. |
| `agent_document_v1.md.tmpl` | `text/template` → pandoc markdown for a document the agent wrote, rendered only from a verified revision: chronology and bordereau tables, inline « À vérifier » warnings, cited sources, verification appendix. |
| `reference-bordereau-v1.docx` | `--reference-doc`: the Word styles the templates address by name. |
| `embed.go` | `//go:embed` of all of the above, plus the reference-doc hash recorded on artifacts. |

Everything interpolated into a `.md.tmpl` is escaped by `services/render.go`
first, so a piece title or a drafted paragraph cannot smuggle markup, an image
reference or raw HTML into pandoc.

## Styles

The templates use pandoc fenced divs (`::: {custom-style="Titre1"}`), which the
docx writer turns into `w:pStyle` references. The reference doc defines:
`Titre1`, `Titre2`, `Titre3`, `Corps`, `Citation`, `Mention`, `PiedDePage`.
`Mention` is the `projet — vérification de l'avocat requise` stamp (§13).

## Replacing the reference doc with a firm's charte

Open `reference-bordereau-v1.docx` in Word, restyle those style ids, save, and
bump `BordereauVersion` in `embed.go`. Keep the ids: `services/render.go`
validates that every style the document uses resolves in `word/styles.xml`, and
refuses to publish a package where one does not.

## Regenerating the reference doc from scratch

It was produced from pandoc's own default reference doc plus the seven style
definitions above:

```sh
pandoc --print-default-data-file reference.docx > base.docx
mkdir x && cd x && unzip -q ../base.docx
# insert the <w:style w:styleId="Titre1">… definitions before </w:styles>
$EDITOR word/styles.xml
# [Content_Types].xml must be the FIRST entry of an OOXML package
zip -X ../reference-bordereau-v1.docx '[Content_Types].xml' && zip -Xr ../reference-bordereau-v1.docx . -x '[Content_Types].xml'
```
