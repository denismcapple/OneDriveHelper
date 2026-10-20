// Command eventmsgs writes the message table that OneDriveHelper.exe carries
// as the OneDriveHelper event source's message file. Every event ID declared
// in internal/elog/elog.go gets a "%1" entry, so Event Viewer shows each
// event's text as written (instead of "The description for Event ID ...
// cannot be found").
//
// It writes one COFF object per architecture (.syso, linked in by go build).
// No Windows SDK (mc.exe, rc.exe) is needed. Run after adding an event ID:
//
//	go generate ./cmd/OneDriveHelper
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Targets are the architectures we build, with their COFF machine type and
// 32-bit image-relative relocation type (ADDR32NB).
var Targets = []struct {
	Arch    string
	Machine uint16
	Reloc   uint16
}{
	{"amd64", 0x8664, 0x0003},
	{"arm64", 0xAA64, 0x0002},
}

// FileName is the .syso written for arch.
func FileName(arch string) string { return "eventmsgs_windows_" + arch + ".syso" }

func main() {
	src := flag.String("elog", filepath.Join("..", "..", "internal", "elog", "elog.go"), "file declaring the event IDs")
	out := flag.String("out", ".", "output folder (cmd/OneDriveHelper)")
	flag.Parse()
	ids, err := EventIDs(*src)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range Targets {
		p := filepath.Join(*out, FileName(t.Arch))
		if err := os.WriteFile(p, Object(t.Machine, t.Reloc, ids), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: %d event IDs\n", p, len(ids))
	}
}

// EventIDs returns the constants of the const block documented "Event IDs.",
// sorted. Each must be a plain integer literal.
func EventIDs(path string) ([]uint32, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	seen := map[uint32]string{}
	var ids []uint32
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST || g.Doc == nil || strings.TrimSpace(g.Doc.Text()) != "Event IDs." {
			continue
		}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				lit, ok := valueAt(vs, i).(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					return nil, fmt.Errorf("%s: %s must be an integer literal", path, n.Name)
				}
				v, err := strconv.ParseUint(lit.Value, 0, 16)
				if err != nil {
					return nil, fmt.Errorf("%s: %s: event IDs must be 0-65535: %v", path, n.Name, err)
				}
				if prev, dup := seen[uint32(v)]; dup {
					return nil, fmt.Errorf("%s: %s and %s are both %d", path, prev, n.Name, v)
				}
				seen[uint32(v)] = n.Name
				ids = append(ids, uint32(v))
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: no event IDs found (a const block documented \"// Event IDs.\")", path)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func valueAt(vs *ast.ValueSpec, i int) ast.Expr {
	if i < len(vs.Values) {
		return vs.Values[i]
	}
	return nil
}

// MessageTable is an RT_MESSAGETABLE resource (MESSAGE_RESOURCE_DATA) with a
// Unicode "%1" entry for each ID; consecutive IDs share a block.
func MessageTable(ids []uint32) []byte {
	var blocks [][2]uint32
	for _, id := range ids {
		if n := len(blocks); n > 0 && blocks[n-1][1]+1 == id {
			blocks[n-1][1] = id
		} else {
			blocks = append(blocks, [2]uint32{id, id})
		}
	}
	text := utf16.Encode([]rune("%1\r\n\x00"))
	entryLen := 4 + 2*len(text)
	entryLen += (4 - entryLen%4) % 4

	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	le(uint32(len(blocks)))
	off := uint32(4 + 12*len(blocks))
	for _, bl := range blocks {
		le([3]uint32{bl[0], bl[1], off})
		off += (bl[1] - bl[0] + 1) * uint32(entryLen)
	}
	for range ids {
		start := b.Len()
		le(uint16(entryLen))
		le(uint16(1)) // MESSAGE_RESOURCE_UNICODE
		le(text)
		for b.Len()-start < entryLen {
			b.WriteByte(0)
		}
	}
	return b.Bytes()
}

// Object is a COFF object with a single .rsrc section holding the message
// table as resource type 11 (RT_MESSAGETABLE), name 1, language en-US.
func Object(machine, reloc uint16, ids []uint32) []byte {
	const (
		dataEntryOff = 72 // after three directories with one entry each
		tableOff     = dataEntryOff + 16
		rawOff       = 20 + 40 // file header + one section header
	)
	table := MessageTable(ids)

	var rsrc bytes.Buffer
	w := func(v any) { _ = binary.Write(&rsrc, binary.LittleEndian, v) }
	dir := func(id, target uint32) { // IMAGE_RESOURCE_DIRECTORY + one ID entry
		w([3]uint32{0, 0, 0}) // Characteristics, TimeDateStamp, Major/MinorVersion
		w([2]uint16{0, 1})    // named entries, ID entries
		w([2]uint32{id, target})
	}
	dir(11, 0x80000000|24)
	dir(1, 0x80000000|48)
	dir(0x0409, dataEntryOff)
	w([4]uint32{tableOff, uint32(len(table)), 0, 0}) // IMAGE_RESOURCE_DATA_ENTRY; OffsetToData is relocated
	rsrc.Write(table)
	for rsrc.Len()%4 != 0 {
		rsrc.WriteByte(0)
	}

	relocOff := uint32(rawOff + rsrc.Len())
	symOff := relocOff + 10
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	name := [8]byte{'.', 'r', 's', 'r', 'c'}
	// IMAGE_FILE_HEADER
	le(machine)
	le(uint16(1)) // NumberOfSections
	le(uint32(0)) // TimeDateStamp (0 keeps the output reproducible)
	le(symOff)    // PointerToSymbolTable
	le(uint32(1)) // NumberOfSymbols
	le(uint16(0)) // SizeOfOptionalHeader
	le(uint16(0)) // Characteristics
	// IMAGE_SECTION_HEADER
	le(name)
	le([2]uint32{0, 0})                       // VirtualSize, VirtualAddress
	le([2]uint32{uint32(rsrc.Len()), rawOff}) // SizeOfRawData, PointerToRawData
	le([2]uint32{relocOff, 0})                // PointerToRelocations, PointerToLinenumbers
	le([2]uint16{1, 0})                       // NumberOfRelocations, NumberOfLinenumbers
	le(uint32(0x40000040))                    // INITIALIZED_DATA | MEM_READ
	b.Write(rsrc.Bytes())
	// IMAGE_RELOCATION: the data entry's OffsetToData, against the .rsrc symbol
	le(uint32(dataEntryOff))
	le(uint32(0))
	le(reloc)
	// IMAGE_SYMBOL: .rsrc, section 1, static
	le(name)
	le(uint32(0)) // Value
	le(uint16(1)) // SectionNumber
	le(uint16(0)) // Type
	le(uint8(3))  // IMAGE_SYM_CLASS_STATIC
	le(uint8(0))  // NumberOfAuxSymbols
	le(uint32(4)) // empty string table
	return b.Bytes()
}
