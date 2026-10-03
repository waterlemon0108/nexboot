package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func Write(rows [][]string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"[Content_Types].xml":        contentTypesXML,
		"_rels/.rels":                relsXML,
		"xl/workbook.xml":            workbookXML,
		"xl/_rels/workbook.xml.rels": workbookRelsXML,
		"xl/worksheets/sheet1.xml":   worksheetXML(rows),
	}
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func Read(data []byte) ([][]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	shared, err := readSharedStrings(zr)
	if err != nil {
		return nil, err
	}
	sheet, err := readZipFile(zr, "xl/worksheets/sheet1.xml")
	if err != nil {
		return nil, err
	}
	return readSheet(sheet, shared)
}

func worksheetXML(rows [][]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r, row := range rows {
		rowNum := r + 1
		fmt.Fprintf(&b, `<row r="%d">`, rowNum)
		for c, value := range row {
			if value == "" {
				continue
			}
			fmt.Fprintf(&b, `<c r="%s%d" t="inlineStr"><is><t>`, colName(c+1), rowNum)
			_ = xml.EscapeText(&b, []byte(value))
			b.WriteString(`</t></is></c>`)
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

func readSharedStrings(zr *zip.Reader) ([]string, error) {
	data, err := readZipFile(zr, "xl/sharedStrings.xml")
	if err != nil {
		if err == zip.ErrFormat || err == io.ErrUnexpectedEOF {
			return nil, err
		}
		return nil, nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var out []string
	var current strings.Builder
	inSI := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			if t.Name.Local == "si" {
				inSI = true
				current.Reset()
			}
		case xml.EndElement:
			if t.Name.Local == "si" {
				out = append(out, current.String())
				inSI = false
			}
		case xml.CharData:
			if inSI {
				current.Write([]byte(t))
			}
		}
	}
	return out, nil
}

func readSheet(data []byte, shared []string) ([][]string, error) {
	var ws worksheet
	if err := xml.Unmarshal(data, &ws); err != nil {
		return nil, err
	}
	var rows [][]string
	for _, row := range ws.Rows {
		values := []string{}
		for _, cell := range row.Cells {
			col := columnIndex(cell.Ref)
			if col <= 0 {
				col = len(values) + 1
			}
			for len(values) < col {
				values = append(values, "")
			}
			values[col-1] = cellValue(cell, shared)
		}
		rows = append(rows, values)
	}
	return rows, nil
}

type worksheet struct {
	Rows []sheetRow `xml:"sheetData>row"`
}

type sheetRow struct {
	Cells []sheetCell `xml:"c"`
}

type sheetCell struct {
	Ref    string    `xml:"r,attr"`
	Type   string    `xml:"t,attr"`
	Value  string    `xml:"v"`
	Inline inlineStr `xml:"is"`
}

type inlineStr struct {
	Text string `xml:"t"`
}

func cellValue(cell sheetCell, shared []string) string {
	switch cell.Type {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(cell.Value))
		if err == nil && i >= 0 && i < len(shared) {
			return shared[i]
		}
		return ""
	case "inlineStr":
		return cell.Inline.Text
	default:
		return cell.Value
	}
}

func readZipFile(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	return nil, nil
}

func colName(n int) string {
	var out []byte
	for n > 0 {
		n--
		out = append([]byte{byte('A' + n%26)}, out...)
		n /= 26
	}
	return string(out)
}

func columnIndex(ref string) int {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A'+1)
	}
	return n
}

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>`

const relsXML = `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

const workbookXML = `<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets><sheet name="terminals" sheetId="1" r:id="rId1"/></sheets>
</workbook>`

const workbookRelsXML = `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`
