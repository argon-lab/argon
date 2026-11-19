package merge

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Use the entire image when JSON cannot uniquely address the original fields.
var errAmbiguousPath = errors.New("BSON field has no unique JSON path")

type exactBytePaths bson.D

func (paths *exactBytePaths) add(path string, value bson.RawValue) {
	*paths = append(*paths, bson.E{Key: path, Value: bson.D{
		{Key: "type", Value: fmt.Sprintf("%02x", byte(value.Type))},
		{Key: "hex", Value: hex.EncodeToString(value.Value)},
	}})
}

func imageExactBytes(raw bson.Raw, name string) (bson.D, error) {
	if raw == nil {
		return nil, nil
	}
	var paths exactBytePaths
	image := bson.RawValue{Type: bson.TypeEmbeddedDocument, Value: raw}
	if err := paths.container(name, image); err != nil {
		if !errors.Is(err, errAmbiguousPath) {
			return nil, err
		}
		paths = nil
		paths.add(name, image)
	}
	return bson.D(paths), nil
}

func (paths *exactBytePaths) container(path string, container bson.RawValue) error {
	fields, err := bson.Raw(container.Value).Elements()
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(fields))
	unorderedArrayKeys := false
	for i, field := range fields {
		key := field.Key()
		if _, duplicate := seen[key]; duplicate || !utf8.ValidString(key) {
			return errAmbiguousPath
		}
		seen[key] = struct{}{}
		if container.Type == bson.TypeArray {
			index := strconv.Itoa(i)
			if key != index {
				unorderedArrayKeys = true
			}
			key = index
		}
		if err := paths.value(path+"."+pathEscapes.Replace(key), field.Value()); err != nil {
			return err
		}
	}
	if unorderedArrayKeys {
		paths.add(path, container)
	}
	return nil
}

func (paths *exactBytePaths) value(path string, value bson.RawValue) error {
	switch value.Type {
	case bson.TypeEmbeddedDocument, bson.TypeArray:
		return paths.container(path, value)
	case bson.TypeCodeWithScope:
		code, scope, ok := value.CodeWithScopeOK()
		if !ok {
			return fmt.Errorf("invalid code with scope at %s", path)
		}
		// The reader discards the code's final byte even if it is not NUL.
		if !utf8.ValidString(code) || value.Value[8+len(code)] != 0 {
			paths.add(path, value)
		}
		return paths.container(path+".$scope", bson.RawValue{Type: bson.TypeEmbeddedDocument, Value: scope})
	case bson.TypeBinary:
		if value.Value[4] == bson.TypeBinaryBinaryOld {
			// A short, lengthless value cannot be reconstructed from JSON.
			if len(value.Value) < 9 || (len(value.Value) == 9 && binary.LittleEndian.Uint32(value.Value[5:9]) != 0) {
				paths.add(path, value)
			} else if uint64(binary.LittleEndian.Uint32(value.Value[5:9])) != uint64(len(value.Value)-9) {
				// Preserve the complete image for conflicting lengths. Older
				// readers can interpret part of this value as another field.
				return errAmbiguousPath
			}
		}
		return nil
	}
	if scalarLosesBytes(value) {
		paths.add(path, value)
	}
	return nil
}

// These are the non-injective scalar encodings in the driver's canonical
// Extended JSON writer and parser. The other scalar tags have a unique textual
// representation or carry all their bits in decimal, hex or base64.
func scalarLosesBytes(value bson.RawValue) bool {
	switch value.Type {
	case bson.TypeDouble:
		f := value.Double()
		return math.IsNaN(f) && math.Float64bits(f) != math.Float64bits(math.NaN())
	case bson.TypeDecimal128:
		d := value.Decimal128()
		decoded, err := primitive.ParseDecimal128(d.String())
		return err != nil || d != decoded
	case bson.TypeString, bson.TypeSymbol, bson.TypeJavaScript:
		// All three store a length followed by the NUL-terminated string.
		return !utf8.Valid(value.Value[4:])
	case bson.TypeDBPointer:
		ns, _, ok := value.DBPointerOK()
		return !ok || !utf8.ValidString(ns)
	case bson.TypeRegex:
		pattern, options := value.Regex()
		return !utf8.ValidString(pattern) || !utf8.ValidString(options) || !slices.IsSorted([]rune(options))
	default:
		return false
	}
}

// ExactBytePathCount returns the number of exact_bytes entries in CLI JSON.
func (p *Plan) ExactBytePathCount() (int, error) {
	return p.countImagePaths(func(raw bson.Raw, name string) (int, error) {
		paths, err := imageExactBytes(raw, name)
		return len(paths), err
	})
}
