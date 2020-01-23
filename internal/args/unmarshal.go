package args

// unmarshal.go helps with the conversion of
// CLI arguments represented as strings
// into CLI arguments represented as Go data.

import (
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"github.com/scaleway/scaleway-sdk-go/strcase"
	"github.com/scaleway/scaleway-sdk-go/validation"
)

type Unmarshaler interface {
	UnmarshalArgs(value string) error
}

type UnmarshalFunc func(value string, dest interface{}) error

var unmarshalFuncs = map[reflect.Type]UnmarshalFunc{
	reflect.TypeOf((*scw.Size)(nil)).Elem(): func(value string, dest interface{}) error {
		// Only support G, GB for now (case insensitive).
		value = strings.ToLower(value)
		if !strings.HasSuffix(value, "g") && !strings.HasSuffix(value, "gb") {
			return fmt.Errorf("size must be defined using the G or GB unit")
		}

		bytes, err := humanize.ParseBytes(value)
		if err != nil {
			return err
		}
		*(dest.(*scw.Size)) = scw.Size(bytes)
		return nil
	},
	reflect.TypeOf((*scw.IPNet)(nil)).Elem(): func(value string, dest interface{}) error {
		return dest.(*scw.IPNet).UnmarshalJSON([]byte(`"` + value + `"`))
	},
	reflect.TypeOf((*io.Reader)(nil)).Elem(): func(value string, dest interface{}) error {
		*(dest.(*io.Reader)) = strings.NewReader(value)
		return nil
	},
}

// UnmarshalStruct parses args like ["arg1=1", "arg2=2"] to a Go structure using reflection.
//
// args: slice of args passed through the command line
// data: Go structure to fill
func UnmarshalStruct(args []string, data interface{}) error {

	// First check if we want to retrieve a simple []string
	if raw, ok := data.(*RawArgs); ok {
		*raw = args
		return nil
	}

	// Second make sure data is a pointer to a struct or a map.
	dest := reflect.ValueOf(data)
	if !(dest.Kind() == reflect.Ptr && (dest.Elem().Kind() == reflect.Struct || dest.Elem().Kind() == reflect.Map)) {
		return &DataMustBeAPointerError{}
	}

	dest = dest.Elem()

	// Map arg names to their values.
	// ["arg1=1", "arg2=2", "arg3"] => [ ["arg1","1"], ["arg2","2"], ["arg3",""] ]
	argsSlice := SplitRaw(args)

	processedArgNames := make(map[string]bool)

	// Loop through all arguments
	for _, kv := range argsSlice {
		argName, argValue := kv[0], kv[1]

		// Make sure argument name is correct.
		// We enforce this check to avoid not well formatted argument name to work by "accident"
		// as we use ToPublicGoName on the argument name later on.
		if !validArgNameRegex.MatchString(argName) {
			err := error(&InvalidArgNameError{})

			// Make an exception for users that try to pass resource UUID without corresponding ID argument.
			// TODO: return a special error to advice user to use the ID argument.
			if validation.IsUUID(argName) {
				err = &UnknownArgError{}
			}

			return &UnmarshalArgError{
				ArgName:  argName,
				ArgValue: argValue,
				Err:      err,
			}
		}

		if processedArgNames[argName] {
			return &UnmarshalArgError{
				ArgName:  argName,
				ArgValue: argValue,
				Err:      &DuplicateArgError{},
			}
		}
		processedArgNames[argName] = true

		// Set will recursively find the correct field to set.
		err := set(dest, strings.Split(argName, "."), argValue)
		if err != nil {
			return &UnmarshalArgError{
				ArgName:  argName,
				ArgValue: argValue,
				Err:      err,
			}
		}
	}

	return nil
}

// IsUmarshalableValue returns true if data type could be unmarshalled with args.UnmarshalValue
func IsUmarshalableValue(data interface{}) bool {
	dest := reflect.ValueOf(data)
	if !dest.IsValid() {
		return false
	}

	for dest.Kind() == reflect.Ptr {
		dest = dest.Elem()
	}

	return isUnmarshalableValue(dest)
}

// RegisterUnmarshalFunc registers an UnmarshalFunc for a given interface.
// i must be a pointer.
func RegisterUnmarshalFunc(i interface{}, unmarshalFunc UnmarshalFunc) {
	unmarshalFuncs[reflect.TypeOf(i).Elem()] = unmarshalFunc
}

// set sets a (sub)value of a data structure.
// It uses reflection to go as deep as necessary into the data struct, following the arg name passed.
//
// dest: the structure to be completed
// argNameWords: the name of the argument to set
// value: the value to be set, represented as a string
//
// Example: argNameWords ["contacts", "0", "address", "city"] will set value "city" for your first contact in your phone book.
func set(dest reflect.Value, argNameWords []string, value string) error {

	// If dest has a custom unmarshaler, we use it.
	// dest can either implement Unmarshaler
	// or have an UnmarshalFunc() registered.
	if isUnmarshalableValue(dest) {
		if len(argNameWords) != 0 {
			// Trying to unmarshal a nested field inside an unmarshalable type
			return &CannotSetNestedFieldError{
				Dest: dest.Interface(),
			}
		}

		for dest.Kind() == reflect.Ptr {
			dest.Set(reflect.New(dest.Type().Elem()))
			dest = dest.Elem()
		}
		return unmarshalValue(value, dest)
	}

	switch dest.Kind() {
	case reflect.Ptr:
		// If type is a nil pointer we create a new Value. NB: maps and slices are pointers.
		if dest.IsNil() {
			dest.Set(reflect.New(dest.Type().Elem()))
		}

		// Call set with the pointer.Elem()
		return set(dest.Elem(), argNameWords, value)

	case reflect.Slice:
		// If type is a slice:
		// We check if argNameWords[0] is an number to handle cases like keys.0.value=12

		// We cannot handle slice without an index notation.
		if len(argNameWords) == 0 {
			return &MissingIndexOnArrayError{}
		}

		// Make sure index is a positive integer.
		index, err := strconv.ParseUint(argNameWords[0], 10, 64)
		if err != nil {
			return &InvalidIndexError{Index: argNameWords[0]}
		}

		// Make sure array is big enough to access the correct index.
		diff := int(index) - dest.Len()
		switch {
		case diff > 0:
			return &MissingIndicesInArrayError{IndexToInsert: int(index), CurrentLength: dest.Len()}
		case diff == 0:
			// Append one element to our slice.
			dest.Set(reflect.AppendSlice(dest, reflect.MakeSlice(dest.Type(), 1, 1)))
		case diff < 0:
			// Element already exist at current index.
		}

		// Recursively call set without the index word
		return set(dest.Index(int(index)), argNameWords[1:], value)

	case reflect.Map:
		// If map is nil we create it.
		if dest.IsNil() {
			dest.Set(reflect.MakeMap(dest.Type()))
		}
		if len(argNameWords) == 0 {
			return &MissingMapKeyError{}
		}
		// Create a new value call set and add result in the map
		newValue := reflect.New(dest.Type().Elem())
		err := set(newValue.Elem(), argNameWords[1:], value)
		dest.SetMapIndex(reflect.ValueOf(argNameWords[0]), newValue.Elem())
		return err

	case reflect.Struct:
		if len(argNameWords) == 0 {
			return &MissingStructFieldError{Dest: dest.Interface()}
		}

		// We cannot rely on dest.GetFieldByName() as reflect library is doing deep traversing when using anonymous field.
		// Because of that we should rely on our own logic
		//
		// - First we try to find a field with the correct name in the current struct
		// - If it does not exist we try to find it in all nested anonymous fields
		//   Anonymous fields are traversed from last to first as the last one in the struct declaration should take precedence

		// We construct two caches:
		anonymousFieldIndexes := []int(nil)
		fieldIndexByName := map[string]int{}
		for i := 0; i < dest.Type().NumField(); i++ {
			field := dest.Type().Field(i)
			if field.Anonymous {
				anonymousFieldIndexes = append(anonymousFieldIndexes, i)
			} else {
				fieldIndexByName[field.Name] = i
			}
		}

		// Try to find the correct field in the current struct.
		fieldName := strcase.ToPublicGoName(argNameWords[0])
		if fieldIndex, exist := fieldIndexByName[fieldName]; exist {
			return set(dest.Field(fieldIndex), argNameWords[1:], value)
		}

		// If it does not exist we try to find it in nested anonymous field
		for i := len(anonymousFieldIndexes) - 1; i >= 0; i-- {
			err := set(dest.Field(anonymousFieldIndexes[i]), argNameWords, value)
			switch err.(type) {
			case nil:
				// If we got no error the field was correctly set we return nil.
				return nil
			case *UnknownArgError:
				// If err is an UnknownArgError this could mean the field is in another anonymous field
				// We continue to the previous anonymous field.
				continue
			default:
				// If we get any other error this mean something went wrong we return an error.
				return err
			}
		}

		// We look in all struct fields + all anonymous fields without success.
		return &UnknownArgError{}
	}
	return &UnmarshalableTypeError{Dest: dest.Interface()}
}

// unmarshalScalar handles unmarshaling from a string to a scalar type .
// It handles transformation like Atoi if dest is an Integer.
func unmarshalScalar(value string, dest reflect.Value) error {
	bitSize := map[reflect.Kind]int{
		reflect.Int:     0,
		reflect.Int8:    8,
		reflect.Int16:   16,
		reflect.Int32:   32,
		reflect.Int64:   64,
		reflect.Uint:    0,
		reflect.Uint8:   8,
		reflect.Uint16:  16,
		reflect.Uint32:  32,
		reflect.Uint64:  64,
		reflect.Float32: 32,
		reflect.Float64: 64,
	}

	switch dest.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(value, 0, bitSize[dest.Kind()])
		dest.SetInt(i)
		return err
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		i, err := strconv.ParseUint(value, 0, bitSize[dest.Kind()])
		dest.SetUint(i)
		return err
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(value, bitSize[dest.Kind()])
		dest.SetFloat(f)
		return err
	case reflect.Bool:
		switch value {
		case "", "true":
			dest.SetBool(true)
		case "false":
			dest.SetBool(false)
		default:
			return fmt.Errorf("invalid boolean value")
		}
		return nil
	case reflect.String:
		dest.SetString(value)
		return nil
	default:
		return &UnmarshalableTypeError{Dest: dest.Interface()}
	}
}

// A type is unmarshalable if:
// - it implement Unmarshaler
// - it has an unmarshalFunc
// - it is a scalar type
func isUnmarshalableValue(dest reflect.Value) bool {

	interface_ := getInterfaceFromReflectValue(dest)

	_, isUnmarshaler := interface_.(Unmarshaler)
	_, hasUnmarshalFunc := unmarshalFuncs[dest.Type()]
	_, isScalar := scalarKinds[dest.Kind()]

	return isUnmarshaler || hasUnmarshalFunc || isScalar
}

func unmarshalValue(value string, dest reflect.Value) error {

	interface_ := getInterfaceFromReflectValue(dest)

	// If src implements Marshaler we call MarshalArgs with the value
	unmarshaler, isUnmarshaler := interface_.(Unmarshaler)
	if isUnmarshaler && unmarshaler != nil {
		return unmarshaler.UnmarshalArgs(value)
	}

	// If src has a registered MarshalFunc(), use it.
	if unmarshalFunc, exists := unmarshalFuncs[dest.Type()]; exists {
		err := unmarshalFunc(value, dest.Addr().Interface())
		if err != nil {
			return &CannotUnmarshalError{
				Dest: dest.Addr().Interface(),
				Err:  err,
			}
		}

		return nil
	}

	if scalarKinds[dest.Kind()] {
		err := unmarshalScalar(value, dest)
		if err != nil {
			return &CannotUnmarshalError{
				Dest: dest.Addr().Interface(),
				Err:  err,
			}
		}

		return nil
	}

	return &CannotUnmarshalError{
		Dest: dest.Interface(),
	}
}