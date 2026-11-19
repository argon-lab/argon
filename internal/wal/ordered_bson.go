package wal

import (
	"bytes"
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

var orderedRegistry = newOrderedRegistry()

func newOrderedRegistry() *bsoncodec.Registry {
	reg := bson.NewRegistry()
	// The v1 value reader counts the inner int32 length as data for an empty
	// subtype-2 binary. Read the raw value so its payload always excludes it.
	binaryType := reflect.TypeFor[primitive.Binary]()
	binaryDecoder, _ := reg.LookupDecoder(binaryType)
	rawDecoder, _ := reg.LookupDecoder(reflect.TypeFor[bson.RawValue]())
	reg.RegisterTypeDecoder(binaryType, bsoncodec.ValueDecoderFunc(func(ctx bsoncodec.DecodeContext, r bsonrw.ValueReader, dst reflect.Value) error {
		if r.Type() != bson.TypeBinary {
			return binaryDecoder.DecodeValue(ctx, r, dst)
		}
		var raw bson.RawValue
		if err := rawDecoder.DecodeValue(ctx, r, reflect.ValueOf(&raw).Elem()); err != nil {
			return err
		}
		subtype, data, ok := raw.BinaryOK()
		if !ok {
			// Older WALs can contain the short, lengthless form accepted
			// by the driver. Limit that decoder to this element's bytes.
			return binaryDecoder.DecodeValue(ctx, bsonrw.NewBSONValueReader(raw.Type, raw.Value), dst)
		}
		dst.Set(reflect.ValueOf(primitive.Binary{Subtype: subtype, Data: data}))
		return nil
	}))
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
