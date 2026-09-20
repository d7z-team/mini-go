package runtime

import "testing"

func FuzzReflectWaitableLifecycle(f *testing.F) {
	f.Add(uint8(0), []byte{0, 1, 2, 3, 4})
	f.Add(uint8(2), []byte{0, 0, 1, 1, 4})
	f.Fuzz(func(t *testing.T, capacity uint8, operations []byte) {
		if len(operations) > 4096 {
			t.Skip()
		}
		module := &moduleInstance{}
		channel, err := makeWaitableValue(module, "Waitable<Int>", newVMValue("Int", int64(capacity%16)))
		if err != nil {
			t.Fatal(err)
		}
		resource := channel.Data.(*waitableResource)
		for index, operation := range operations {
			switch operation % 5 {
			case 0:
				_, _ = waitableTrySendValue(module, channel, newVMValue("Int", int64(index)))
			case 1:
				_, _, _, _ = waitableTryRecvValue(module, channel)
			case 2, 3:
				selection, err := (*vm)(nil).prepareChannelSelection(nil, module, []channelSelectCase{{channel: channel, send: operation%5 == 3, value: newVMValue("Int", int64(index))}})
				if err != nil {
					t.Fatal(err)
				}
				if !selection.tryCommit() {
					selection.register()
				}
				selection.unregister()
				if resource.selectHead != nil || resource.selectTail != nil {
					t.Fatal("canceled selection retained registrations")
				}
			case 4:
				_ = waitableCloseValue(module, channel)
			}
			if len(resource.Buffer) > resource.Capacity {
				t.Fatalf("buffer length %d exceeds capacity %d", len(resource.Buffer), resource.Capacity)
			}
		}
	})
}
