package providerapi

import (
	"encoding/base64"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const kiroMaxDocumentBytes = 20 << 20

type kiroDocument struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

var kiroDocumentFormats = map[string]string{
	"application/pdf": "pdf", "text/plain": "txt", "text/csv": "csv", "text/html": "html", "text/markdown": "md",
	"application/msword": "doc", "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
	"application/vnd.ms-excel": "xls", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "xlsx",
}

// Inline documents follow the Kiro document wire used by kiro-reverse-api.
// Unresolvable IDs/URLs are rejected instead of silently discarding a file.
func kiroDocumentBlock(block gjson.Result) (kiroDocument, error) {
	var doc kiroDocument
	name := block.Get("title").String()
	if name == "" {
		name = block.Get("filename").String()
	}
	if name == "" {
		name = block.Get("file.filename").String()
	}
	media, data := block.Get("source.media_type").String(), block.Get("source.data").String()
	switch block.Get("source.type").String() {
	case "text":
		if !utf8.ValidString(data) || len(data) > kiroMaxDocumentBytes {
			return doc, fmt.Errorf("invalid or oversized Kiro text document")
		}
		media = "text/plain"
		data = base64.StdEncoding.EncodeToString([]byte(data))
	case "base64":
	case "":
		data = block.Get("file_data").String()
		if data == "" {
			data = block.Get("file.file_data").String()
		}
	default:
		return doc, fmt.Errorf("Kiro requires inline document data")
	}
	if strings.HasPrefix(data, "data:") {
		header, payload, ok := strings.Cut(data, ",")
		if !ok || !strings.HasSuffix(strings.ToLower(header), ";base64") {
			return doc, fmt.Errorf("invalid Kiro document data URL")
		}
		media = strings.TrimPrefix(header[:len(header)-len(";base64")], "data:")
		data = payload
	}
	if data == "" || len(data) > base64.StdEncoding.EncodedLen(kiroMaxDocumentBytes) {
		return doc, fmt.Errorf("missing or oversized Kiro document data")
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(decoded) > kiroMaxDocumentBytes {
		return doc, fmt.Errorf("invalid Kiro document base64")
	}
	media = strings.ToLower(strings.TrimSpace(strings.Split(media, ";")[0]))
	doc.Format = kiroDocumentFormats[media]
	if doc.Format == "" && media == "" {
		ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
		for _, format := range kiroDocumentFormats {
			if ext == format {
				doc.Format = format
				break
			}
		}
	}
	if doc.Format == "" {
		return doc, fmt.Errorf("unsupported Kiro document format")
	}
	doc.Name = kiroDocumentName(name)
	doc.Source.Bytes = data
	return doc, nil
}

func kiroDocumentName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-()[]", r) {
			return r
		}
		return ' '
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if len(name) > 90 {
		name = strings.TrimSpace(name[:90])
	}
	if name == "" {
		name = "document"
	}
	return name
}

func uniqueKiroDocumentNames(docs []kiroDocument) {
	used := map[string]bool{}
	for i := range docs {
		base := docs[i].Name
		for n := 2; used[docs[i].Name]; n++ {
			docs[i].Name = fmt.Sprintf("%s (%d)", base, n)
		}
		used[docs[i].Name] = true
	}
}

func kiroToolResultContent(content gjson.Result, user *kiroUser) (string, error) {
	if !content.Exists() || content.Type == gjson.String {
		return content.String(), nil
	}
	if !content.IsArray() {
		return "", fmt.Errorf("invalid Kiro tool result content")
	}
	var parts []string
	for _, block := range content.Array() {
		switch block.Get("type").String() {
		case "text":
			parts = append(parts, block.Get("text").String())
		case "document", "file", "input_file":
			doc, err := kiroDocumentBlock(block)
			if err != nil {
				return "", err
			}
			user.Documents = append(user.Documents, doc)
			parts = append(parts, "[Tool returned a document; the document is attached to this message.]")
		default:
			return "", fmt.Errorf("unsupported Kiro tool result content block")
		}
	}
	return strings.Join(parts, "\n"), nil
}
