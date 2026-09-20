package runtime

import (
	"errors"
	"fmt"
	"io"
)

func cryptoRandRead(ctx intrinsicContext, args []vmValue) ([]vmValue, error) {
	slice, ok := args[0].Data.(*vmSlice)
	if !ok && args[0].Data != nil {
		return nil, errors.New("crypto/rand intrinsic requires []byte")
	}
	if slice == nil || slice.Len == 0 {
		return cryptoRandReadResult(0, nil), nil
	}

	data := make([]byte, slice.Len)
	ctx.vm.hostMu.Lock()
	n, err := ctx.vm.entropy.Read(data)
	ctx.vm.hostMu.Unlock()
	if n < 0 || n > len(data) {
		err = fmt.Errorf("crypto/rand: invalid read count %d", n)
		n = 0
	} else if n == 0 && err == nil {
		err = io.ErrNoProgress
	}
	slice.writeBytes(0, data[:n])
	return cryptoRandReadResult(n, err), nil
}

func cryptoRandReadResult(n int, err error) []vmValue {
	message := ""
	ok := err == nil
	if err != nil {
		message = err.Error()
	}
	return []vmValue{
		newVMValue("Int", int64(n)),
		newVMValue("String", message),
		newVMValue("Bool", ok),
	}
}
