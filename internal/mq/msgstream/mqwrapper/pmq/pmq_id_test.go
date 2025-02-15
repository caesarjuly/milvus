// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pmq

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPmqID_Serialize(t *testing.T) {
	rid := &pmqID{
		messageID: 8,
	}

	bin := rid.Serialize()
	assert.NotNil(t, bin)
	assert.NotZero(t, len(bin))
}

func Test_AtEarliestPosition(t *testing.T) {
	rid := &pmqID{
		messageID: 0,
	}
	assert.True(t, rid.AtEarliestPosition())

	rid = &pmqID{
		messageID: math.MaxInt64,
	}
	assert.False(t, rid.AtEarliestPosition())
}

func TestLessOrEqualThan(t *testing.T) {
	rid1 := &pmqID{
		messageID: 0,
	}
	rid2 := &pmqID{
		messageID: math.MaxInt64,
	}

	ret, err := rid1.LessOrEqualThan(rid2.Serialize())
	assert.Nil(t, err)
	assert.True(t, ret)

	ret, err = rid2.LessOrEqualThan(rid1.Serialize())
	assert.Nil(t, err)
	assert.False(t, ret)

	ret, err = rid1.LessOrEqualThan(rid1.Serialize())
	assert.Nil(t, err)
	assert.True(t, ret)
}

func Test_Equal(t *testing.T) {
	rid1 := &pmqID{
		messageID: 0,
	}

	rid2 := &pmqID{
		messageID: math.MaxInt64,
	}

	{
		ret, err := rid1.Equal(rid1.Serialize())
		assert.Nil(t, err)
		assert.True(t, ret)

	}

	{
		ret, err := rid1.Equal(rid2.Serialize())
		assert.Nil(t, err)
		assert.False(t, ret)
	}
}

func Test_SerializePmqID(t *testing.T) {
	bin := SerializePmqID(10)
	assert.NotNil(t, bin)
	assert.NotZero(t, len(bin))
}

func Test_DeserializePmqID(t *testing.T) {
	bin := SerializePmqID(5)
	id := DeserializePmqID(bin)
	assert.Equal(t, id, int64(5))
}
