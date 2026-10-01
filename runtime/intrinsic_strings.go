package runtime

import (
	"errors"
	"strings"
)

func stringsIndexByte(_ intrinsicContext, args []vmValue) ([]vmValue, error) {
	text, ok := args[0].Data.(string)
	start, startErr := asInt64(args[1])
	end, endErr := asInt64(args[2])
	needle, needleErr := asInt64(args[3])
	if !ok || startErr != nil || endErr != nil || needleErr != nil || start < 0 || end < start || end > int64(len(text)) || end-start > 4096 || needle < 0 || needle > 255 {
		return nil, errors.New("strings.index_byte: invalid bounded search")
	}
	offset := strings.IndexByte(text[start:end], byte(needle))
	if offset >= 0 {
		offset += int(start)
	}
	return []vmValue{newVMValue("Int", int64(offset))}, nil
}
