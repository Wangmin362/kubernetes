/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package recognizer

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// RecognizingDecoder
// 1、由于K8S支持YAML, JSON， Protobuf三种数据，因此二进制数据可能是这三种数据中的一种。因此，当我们拿到一个二进制数据之后，我们就需要识别
// 这个数据，并且使用合适的解码器进行解码。我们如何判断数据是否可以使用当前解码器进行解码的这个功能就是RecognizesData接口意义所在。
// 2、RecognizesData接口的RecognizesData方法就是为了判断当前解码器是否能够解码指定的数据
// 3、实现此接口的只有JSON，Protobuf类型的解码器，那么为什么没有YAML类型的解码器呢？ 原因是因为JSON是YAML的子集，JSON解码器就可以解码
type RecognizingDecoder interface {
	runtime.Decoder
	// RecognizesData should return true if the input provided in the provided reader
	// belongs to this decoder, or an error if the data could not be read or is ambiguous.
	// Unknown is true if the data could not be determined to match the decoder type.
	// Decoders should assume that they can read as much of peek as they need (as the caller
	// provides) and may return unknown if the data provided is not sufficient to make a
	// a determination. When peek returns EOF that may mean the end of the input or the
	// end of buffered input - recognizers should return the best guess at that time.
	// 1、RecognizesData的返回值比较复杂，我们详细说明一下：
	// 2、如果一个解码器明确知道自己无法解码peek数据，那么该解码器应该返回：ok=false, unknown=false, err=nil
	// 3、如果一个解码器明确知道自己可以解码peek数据，那么该解码器应该返回：ok=true, unknown=false, err=nil
	// 4、如果一个解码器不清楚自己是否可以解码peek数据，那么该解码器应该返回：ok=false, unknown=true, err=nil。需要注意的是，这种情况下，
	// 并不是说该解码器就一定无法解码peek数据，只是由于数据特征不明显，解码器无法判断自己到底能否解码peek数据，很有可能当前解码器是可以解码peek
	// 数据的。再K8S当中，YAML类型的数据就没有明确的特征，因此YAML类型的解码器就算碰到了YAML数据也无法判断自己到底能够解码，只能返回unknown=true
	// 5、需要注意的是，识别peek数据是否能够被当前解码器解码的办法不应该是直接进行反序列化，这样的成本太高了。而是应该根据数据的某些特征判断
	// 是否能够解码，譬如对于JSON数据来说，{}花括号就是特征，由于K8S中不存在数据，所有的数据都是对象，即便是列表数据，也有专门的XXXList对象，
	// 所以花括号就是JSON数据的特征。而对于Protobuf来说，数据的前面几个字节是0x6b, 0x38, 0x73, 0x00也是固定的。根据特征识别当前解码器能否
	// 解码peek数据是一种比较取巧的方式，但是非常有效，而且成本很低，无需消耗过多的CPU。这也是为什么参数被称之为peek，因为RecognizesData只
	// 需要看一眼数据就知道能否解析，而无需知道数据的全貌。
	RecognizesData(peek []byte) (ok, unknown bool, err error)
}

// NewDecoder creates a decoder that will attempt multiple decoders in an order defined
// by:
//
// 1. The decoder implements RecognizingDecoder and identifies the data
// 2. All other decoders, and any decoder that returned true for unknown.
//
// The order passed to the constructor is preserved within those priorities.
// 我们再注册解码器的时候应该把实现了RecognizesData的解码器放在前面，把没有实现RecognizesData接口的解码器放在后面，
// 这样可以尽可能的减少CPU硬解码开销
func NewDecoder(decoders ...runtime.Decoder) runtime.Decoder {
	return &decoder{
		decoders: decoders,
	}
}

// 1、decoder可以称之为万能解码器，对于K8S中的任意数据，我们都可以使用decoder进行解码。
// 2、decoder实现原理非常简单，其内部只需要包含多个解码器，然后我们解码的时候，只需要挨个遍历每个解码器，看看能否解码即可。
type decoder struct {
	// 之所以没有使用RecognizingDecoder接口，是因为可能会直接硬解码数据，所以我们再注册解码器的时候应该把实现了RecognizesData的解码器
	// 放在前面，把没有实现RecognizesData接口的解码器放在后面，这样可以尽可能的减少CPU硬解码开销
	decoders []runtime.Decoder
}

var _ RecognizingDecoder = &decoder{}

// RecognizesData 是否可以识别这个二进制数据，如果能够识别，意味着可以正确进行反序列化
func (d *decoder) RecognizesData(data []byte) (bool, bool, error) {
	var (
		lastErr    error
		anyUnknown bool // 只要有其中任何一个解码器返回unknown，这个数据就还有解码的可能
	)
	// 遍历所有的解码器
	for _, r := range d.decoders {
		switch t := r.(type) {
		// 如果当前解码器是RecognizingDecoder类型，那么该解码器就有能力判断
		case RecognizingDecoder:
			ok, unknown, err := t.RecognizesData(data)
			if err != nil {
				lastErr = err
				continue
			}
			// 只要有其中任何一个解码器返回unknown，就返回unknown
			anyUnknown = anyUnknown || unknown
			if !ok {
				continue
			}
			// 只要有其中任何一个解码器能够识别当前数据，就直接返回
			return true, false, nil
		}
	}
	return false, anyUnknown, lastErr
}

func (d *decoder) Decode(data []byte, gvk *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	var (
		lastErr error
		skipped []runtime.Decoder
	)

	// try recognizers, record any decoders we need to give a chance later
	// 遍历所有的解码器，挨个看看每个解码器能否解码当前数据
	for _, r := range d.decoders {
		switch t := r.(type) {
		// 如果当前解码器是RecognizingDecoder类型，那么我们可以先看看当前解码器是否有能力解码当前的数据
		case RecognizingDecoder:
			ok, unknown, err := t.RecognizesData(data)
			if err != nil {
				lastErr = err
				continue
			}
			// 如果当前解码器说不知道能不能解码当前的数据，就把跳过当前解码器，后续可能还是会使用这个解码器进行解码，因为我们前面已经详细研究
			// 过了unknown的含义，它标识当前数据的特征并不明显，解码器无法足够的信息判断自己是否能够解码数据。有可能是可以加密的，譬如YAML解码器
			if unknown {
				skipped = append(skipped, t)
				continue
			}
			// 如果当前解码器已经明确说了自己无法解码这个数据，那就直接忽略这个解码器
			if !ok {
				continue
			}
			// 如果当前解码器明确说明自己可以解码数据，那就直接通过这个解码器进行解码
			return r.Decode(data, gvk, into)
		default:
			// 如果当前解码器没有实现RecognizesData，直接缓存这个解码器，后续可能会使用这个解码器直接硬解码
			skipped = append(skipped, t)
		}
	}

	// try recognizers that returned unknown or didn't recognize their data
	// 如果所有的解码器没有一个明确说明可以明确加密当前数据，那么只能再尝试哪些说unknown的解码器，这些解码器有可能是可以正确解码的
	for _, r := range skipped {
		// 直接硬解码数据，没有错误就认为解码成功
		out, actual, err := r.Decode(data, gvk, into)
		if err != nil {
			// if we got an object back from the decoder, and the
			// error was a strict decoding error (e.g. unknown or
			// duplicate fields), we still consider the recognizer
			// to have understood the object
			if out == nil || !runtime.IsStrictDecodingError(err) {
				lastErr = err
				continue
			}
		}
		return out, actual, err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no serialization format matched the provided data")
	}
	return nil, nil, lastErr
}
