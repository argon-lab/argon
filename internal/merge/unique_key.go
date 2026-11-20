package merge

import (
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
)

// uniqueKeyToken encodes equality, not sort order. BSON scalar encodings
// already delimit their values; compound keys can concatenate them directly.
func uniqueKeyToken(values []bson.RawValue) (string, error) {
	var key []byte
	for _, value := range values {
		var err error
		key, err = appendUniqueValue(key, value)
		if err != nil {
			return "", err
		}
	}
	return string(key), nil
}

func appendUniqueValue(key []byte, value bson.RawValue) ([]byte, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	switch value.Type {
	case bson.TypeDouble, bson.TypeInt32, bson.TypeInt64, bson.TypeDecimal128:
		number, err := indexNumber(value)
		if err != nil {
			return nil, err
		}
		key = append(key, byte(bson.TypeDouble))
		key = append(key, number...)
		return append(key, 0), nil
	case bson.TypeSymbol:
		value.Type = bson.TypeString
	case bson.TypeEmbeddedDocument, bson.TypeArray:
		fields, err := bson.Raw(value.Value).Elements()
		if err != nil {
			return nil, err
		}
		key = append(key, byte(value.Type))
		key = binary.AppendUvarint(key, uint64(len(fields)))
		for _, field := range fields {
			if value.Type == bson.TypeEmbeddedDocument {
				key = append(key, field.Key()...)
				key = append(key, 0)
			}
			key, err = appendUniqueValue(key, field.Value())
			if err != nil {
				return nil, err
			}
		}
		return key, nil
	case bson.TypeCodeWithScope:
		code, scope, ok := value.CodeWithScopeOK()
		if !ok {
			return nil, errors.New("invalid BSON code with scope")
		}
		key = append(key, byte(value.Type))
		key = binary.AppendUvarint(key, uint64(len(code)))
		key = append(key, code...)
		return appendUniqueValue(key, bson.RawValue{Type: bson.TypeEmbeddedDocument, Value: scope})
	case bson.TypeDBPointer:
		namespace, id, ok := value.DBPointerOK()
		if !ok {
			return nil, errors.New("invalid BSON DBPointer")
		}
		// MongoDB's index encoder reads the namespace as a C string.
		namespace, _, _ = strings.Cut(namespace, "\x00")
		key = append(key, byte(value.Type))
		key = append(key, namespace...)
		key = append(key, 0)
		return append(key, id[:]...), nil
	}
	key = append(key, byte(value.Type))
	return append(key, value.Value...), nil
}

func indexNumber(value bson.RawValue) (string, error) {
	var coefficient *big.Int
	var exponent int
	switch value.Type {
	case bson.TypeDouble:
		f := value.Double()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'g', -1, 64), nil
		}
		// For n / 2^k the exact decimal is (n * 5^k) * 10^-k.
		r := new(big.Rat).SetFloat64(f)
		k := r.Denom().BitLen() - 1
		exponent = -k
		coefficient = new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(k)), nil)
		coefficient.Mul(coefficient, r.Num())
	case bson.TypeDecimal128:
		d := value.Decimal128()
		if d.IsNaN() {
			return "NaN", nil
		}
		if sign := d.IsInf(); sign != 0 {
			return strconv.FormatFloat(math.Inf(sign), 'g', -1, 64), nil
		}
		var err error
		coefficient, exponent, err = d.BigInt()
		if err != nil {
			return "", err
		}
	default:
		coefficient = big.NewInt(value.AsInt64())
	}
	if coefficient.Sign() == 0 {
		return "0", nil
	}
	digits := coefficient.String()
	// IEEE 754 interprets an out-of-range decimal coefficient as zero.
	if value.Type == bson.TypeDecimal128 && len(strings.TrimPrefix(digits, "-")) > 34 {
		return "0", nil
	}
	trimmed := strings.TrimRight(digits, "0")
	return trimmed + "e" + strconv.Itoa(exponent+len(digits)-len(trimmed)), nil
}
