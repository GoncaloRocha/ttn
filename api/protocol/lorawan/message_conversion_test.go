// Copyright © 2016 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package lorawan

import (
	"testing"

	"github.com/brocaar/lorawan"
	. "github.com/smartystreets/assertions"
)

func TestConvertPHYPayload(t *testing.T) {
	a := New(t)

	{
		m1 := Message{Mic: []byte{0, 0, 0, 0}}
		m1.MType = MType_UNCONFIRMED_UP
		macPayload := MACPayload{}
		macPayload.FOpts = []MACCommand{
			MACCommand{Cid: 0x02},
		}
		m1.Payload = &Message_MacPayload{MacPayload: &macPayload}
		phy := m1.PHYPayload()
		m2 := MessageFromPHYPayload(phy)
		a.So(m2, ShouldResemble, m1)
	}

	{
		m1 := Message{Mic: []byte{0, 0, 0, 0}}
		m1.MType = MType_JOIN_REQUEST
		joinRequestPayload := JoinRequestPayload{}
		m1.Payload = &Message_JoinRequestPayload{JoinRequestPayload: &joinRequestPayload}
		phy := m1.PHYPayload()
		m2 := MessageFromPHYPayload(phy)
		a.So(m2, ShouldResemble, m1)
	}

	{
		m1 := Message{Mic: []byte{0, 0, 0, 0}}
		m1.MType = MType_JOIN_ACCEPT
		joinAcceptPayload := JoinAcceptPayload{}
		joinAcceptPayload.CfList = &CFList{
			Freq: []uint32{867100000, 867300000, 867500000, 867700000, 867900000},
		}
		m1.Payload = &Message_JoinAcceptPayload{JoinAcceptPayload: &joinAcceptPayload}
		phy := m1.PHYPayload()
		m2 := MessageFromPHYPayload(phy)
		a.So(m2, ShouldResemble, m1)

		phy.MACPayload = &lorawan.DataPayload{Bytes: []byte{0x01, 0x02, 0x03, 0x04}}

		m3 := MessageFromPHYPayload(phy)

		phy = m3.PHYPayload()
	}

}
