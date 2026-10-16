FROM golang:1.26.2-alpine AS builder
WORKDIR /api
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /out/api .

FROM alpine:3.20
# Document-processing CLIs (docs/PLAN.md §7.2, §7.3). These are invoked as
# subprocesses through utils.RunTool, never linked, so the Go binary stays
# CGO_ENABLED=0:
#   poppler-utils           pdftotext (text-layer extraction) + pdftoppm (page rasterisation)
#   tesseract-ocr + …-fra   local French OCR — the DEFAULT processing mode; remote
#                           vision is admin-gated and allowlisted, never a fallback
#   pandoc-cli              the single DOCX renderer (~220 MB installed)
# Only the `fra` traineddata is installed, so every tesseract invocation MUST
# pass `-l fra` — tesseract's implicit default is `eng` and would fail here.
# Versions are fuzzy-pinned to the minor shipped by the alpine 3.20 repos, so
# a rebuild picks up branch security patches but never a surprise major.
RUN apk add --no-cache \
      ca-certificates \
      poppler-utils~=24.02 \
      tesseract-ocr~=5.3 \
      tesseract-ocr-data-fra~=5.3 \
      pandoc-cli~=0.1.1
COPY --from=builder /out/api /api
EXPOSE 8000
ENTRYPOINT ["/api"]
