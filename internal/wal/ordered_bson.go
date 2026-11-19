package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/bsonrw"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// MarshalDocument preserves the BSON order of ordered inputs and sorts
// fields supplied as Go maps, including those inside arrays and structs.
func MarshalDocument(doc any) (bson.Raw, error) {
	var buf bytes.Buffer
	writer, err := bsonrw.NewBSONValueWriter(&buf)
	if err != nil {
		return nil, err
	}
	encoder, err := bson.NewEncoder(writer)
	if err != nil {
		return nil, err
	}
	if err := encoder.SetRegistry(orderedRegistry); err != nil {
		return nil, err
	}
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalDocument decodes a stored image, including BSON's old binary subtype.
func UnmarshalDocument(raw bson.Raw, dest any) error {
	dec, err := bson.NewDecoder(bsonrw.NewBSONDocumentReader(raw))
	if err != nil {
		return err
	}
	if err := dec.SetRegistry(orderedRegistry); err != nil {
		return err
	}
	return dec.Decode(dest)
}

// UnmarshalValue uses the same decoder for a single BSON element.
func UnmarshalValue(raw bson.RawValue, dest any) error {
	return raw.UnmarshalWithRegistry(orderedRegistry, dest)
}

var (
	orderedRegistry = newOrderedRegistry(false)
	keyRegistry     = newOrderedRegistry(true)
	errOldBinaryKey = errors.New("BSON document key contains subtype 2")
)

// ErrOldBinaryLength means the inner and outer lengths of a subtype-2
// value describe different payloads.
var ErrOldBinaryLength = errors.New("inconsistent BSON subtype-2 binary length")

func newOrderedRegistry(detectOldBinaryKey bool) *bsoncodec.Registry {
	reg := bson.NewRegistry()
	// The v1 value reader counts the inner int32 length as data for an empty
	// subtype-2 binary. Read the raw value so its payload always excludes it.
	binaryType := reflect.TypeFor[primitive.Binary]()
	binaryDecoder, _ := reg.LookupDecoder(binaryType)
	rawDecoder, _ := reg.LookupDecoder(reflect.TypeFor[bson.RawValue]())
	reg.RegisterTypeDecoder(binaryType, bsoncodec.ValueDecoderFunc(func(ctx bsoncodec.DecodeContext, r bsonrw.ValueReader, dst reflect.Value) (err error) {
		if r.Type() != bson.TypeBinary {
			return binaryDecoder.DecodeValue(ctx, r, dst)
		}
		// Copying the raw value panics in the driver on a negative length.
		// Bytes from a socket can carry one; report them as invalid BSON.
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("invalid BSON binary value: %v", p)
			}
		}()
		var raw bson.RawValue
		if err := rawDecoder.DecodeValue(ctx, r, reflect.ValueOf(&raw).Elem()); err != nil {
			return err
		}
		if len(raw.Value) < 5 || uint64(binary.LittleEndian.Uint32(raw.Value)) != uint64(len(raw.Value)-5) {
			return fmt.Errorf("invalid BSON binary length")
		}
		subtype, data := raw.Value[4], raw.Value[5:]
		if subtype == bson.TypeBinaryBinaryOld {
			// A historical lengthless payload can be up to four bytes.
			// Four zero bytes encode the standard empty payload.
			if len(data) >= 4 {
				inner := binary.LittleEndian.Uint32(data)
				if uint64(inner) == uint64(len(data)-4) {
					data = data[4:]
				} else if len(data) > 4 {
					return fmt.Errorf("%w: inner count %d, payload %d", ErrOldBinaryLength, inner, len(data)-4)
				}
			}
			// Detect the subtype on the first key decode, including nested
			// fields. Only an affected key needs another decode.
			if detectOldBinaryKey {
				return errOldBinaryKey
			}
		}
		dst.Set(reflect.ValueOf(primitive.Binary{Subtype: subtype, Data: data}))
		return nil
	}))
	if detectOldBinaryKey {
		return reg
	}
	// Keep the driver's handling of BSON tags, custom encoders and map keys.
	// Only the order in which map fields are emitted changes.
	type mapSample map[string]any
	type structSample struct{}
	mapEncoder, _ := reg.LookupEncoder(reflect.TypeFor[mapSample]())
	structEncoder, _ := reg.LookupEncoder(reflect.TypeFor[structSample]())

	reg.RegisterKindEncoder(reflect.Map, bsoncodec.ValueEncoderFunc(func(ctx bsoncodec.EncodeContext, w bsonrw.ValueWriter, v reflect.Value) error {
		if v.IsNil() {
			return mapEncoder.EncodeValue(ctx, w, v)
		}
		if v.Type().Key().Kind() != reflect.String {
			return encodeSortedSuffix(ctx, w, v, mapEncoder, v.Len())
		}
		fields := make(bson.D, 0, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			fields = append(fields, bson.E{Key: iter.Key().String(), Value: iter.Value().Interface()})
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
		return encodeFields(ctx, w, fields)
	}))
	reg.RegisterKindEncoder(reflect.Struct, bsoncodec.ValueEncoderFunc(func(ctx bsoncodec.EncodeContext, w bsonrw.ValueWriter, v reflect.Value) error {
		// The struct codec appends an inline map directly to the struct's
		// fields, bypassing the map encoder. Only that suffix needs sorting.
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() || field.Type.Kind() != reflect.Map {
				continue
			}
			tag, tagged := field.Tag.Lookup("bson")
			if !tagged && !strings.Contains(string(field.Tag), ":") {
				tag = string(field.Tag)
			}
			if slices.Contains(strings.Split(tag, ","), "inline") {
				return encodeSortedSuffix(ctx, w, v, structEncoder, v.Field(i).Len())
			}
		}
		return structEncoder.EncodeValue(ctx, w, v)
	}))
	return reg
}

func encodeFields(ctx bsoncodec.EncodeContext, w bsonrw.ValueWriter, fields bson.D) error {
	value := reflect.ValueOf(fields)
	enc, err := ctx.LookupEncoder(value.Type())
	if err != nil {
		return err
	}
	return enc.EncodeValue(ctx, w, value)
}

// Use the driver's own encoding for fields whose BSON names come from a
// struct tag or a custom map key. Moving raw values preserves their bytes.
func encodeSortedSuffix(ctx bsoncodec.EncodeContext, w bsonrw.ValueWriter, v reflect.Value, encoder bsoncodec.ValueEncoder, count int) error {
	if count < 2 {
		return encoder.EncodeValue(ctx, w, v)
	}
	var buf bytes.Buffer
	tmp, err := bsonrw.NewBSONValueWriter(&buf)
	if err != nil {
		return err
	}
	if err := encoder.EncodeValue(ctx, tmp, v); err != nil {
		return err
	}
	elements, err := bson.Raw(buf.Bytes()).Elements()
	if err != nil {
		return err
	}
	fields := make(bson.D, len(elements))
	for i, element := range elements {
		fields[i] = bson.E{Key: element.Key(), Value: element.Value()}
	}
	tail := fields[len(fields)-count:]
	sort.Slice(tail, func(i, j int) bool { return tail[i].Key < tail[j].Key })
	return encodeFields(ctx, w, fields)
}
