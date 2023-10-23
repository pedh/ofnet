// #nosec G404: random number generator not used for security purposes
package ofctrl

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"testing"
	"time"

	"antrea.io/libOpenflow/openflow15"
	"antrea.io/libOpenflow/protocol"
	"antrea.io/libOpenflow/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type packetApp struct {
	*OfActor
	pktCh chan *PacketIn
}

func (p *packetApp) PacketRcvd(sw *OFSwitch, pkt *PacketIn) {
	p.pktInCount += 1
	p.pktCh <- pkt
}

func TestGetNXRangeFromUint32Mask(t *testing.T) {
	r := openflow15.NewNXRange(0, 4)
	oriOfsNbits := r.ToOfsBits()
	mask := r.ToUint32Mask()
	r2 := getNXRangeFromUint32Mask(mask)
	newOfsNbits := r2.ToOfsBits()
	assert.Equal(t, oriOfsNbits, newOfsNbits)
}

func TestPacketIn_PacketOut(t *testing.T) {
	app := new(packetApp)
	app.OfActor = new(OfActor)
	app.pktCh = make(chan *PacketIn)
	ctrl := NewController(app)
	brName := "br4pkt"
	ovsBr := prepareControllerAndSwitch(t, app.OfActor, ctrl, brName)
	defer func() {
		assert.Nilf(t, ovsBr.DeleteBridge(brName), "Failed to delete br %s", brName)
		ctrl.Delete()
	}()
	app.Switch.EnableMonitor()
	for _, tc := range []struct {
		name         string
		isIPv6       bool
		controllerv2 bool
		reason       uint8
		userData     []byte
		tcpDst       uint16
		pause        bool
	}{
		{
			name:         "ipv4-controller",
			isIPv6:       false,
			controllerv2: false,
			reason:       1,
			tcpDst:       1001,
		}, {
			name:         "ipv6-controller",
			isIPv6:       true,
			controllerv2: false,
			reason:       1,
			tcpDst:       1002,
		}, {
			name:         "ipv4-controller2-no-userdata",
			isIPv6:       false,
			controllerv2: true,
			reason:       1,
			tcpDst:       1003,
		}, {
			name:         "ipv6-controller2-no-userdata",
			isIPv6:       true,
			controllerv2: true,
			reason:       1,
			tcpDst:       1004,
		}, {
			name:         "ipv4-controller2-userdata",
			isIPv6:       false,
			controllerv2: true,
			reason:       1,
			tcpDst:       1005,
			userData:     []byte{1, 2, 3},
		}, {
			name:         "ipv6-controller2-userdata",
			isIPv6:       true,
			controllerv2: true,
			reason:       1,
			tcpDst:       1005,
			userData:     []byte{4, 5, 6},
		}, {
			name:         "ipv4-controller2-pause-resume",
			isIPv6:       false,
			controllerv2: true,
			reason:       1,
			tcpDst:       1005,
			userData:     []byte{1, 2, 3},
			pause:        true,
		}, {
			name:         "ipv6-controller2-pause-resume",
			isIPv6:       true,
			controllerv2: true,
			reason:       1,
			tcpDst:       1005,
			pause:        true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testPacketInOut(t, app, tc.isIPv6, tc.reason, tc.controllerv2, tc.tcpDst, tc.userData, tc.pause)
		})
	}
}

func TestNxOutputAndSendController(t *testing.T) {
	app := new(packetApp)
	app.OfActor = new(OfActor)
	app.pktCh = make(chan *PacketIn)
	ctrl := NewController(app)
	brName := "br4sendcontroller"
	ovsBr := prepareControllerAndSwitch(t, app.OfActor, ctrl, brName)
	defer func() {
		assert.Nilf(t, ovsBr.DeleteBridge(brName), "Failed to delete br %s", brName)
		ctrl.Delete()
	}()

	app.Switch.EnableMonitor()
	ofSwitch := app.Switch
	table0 := ofSwitch.DefaultTable()
	srcMAC, _ := net.ParseMAC("11:22:33:44:55:66")
	flow1 := &Flow{
		Table: table0,
		Match: FlowMatch{
			Priority:  100,
			Ethertype: 0x0800,
			MacSa:     &srcMAC,
		},
	}
	err := flow1.OutputReg("NXM_NX_REG0", 0, 31)
	require.Nil(t, err)
	err = flow1.Controller(0x1)
	require.Nil(t, err)
	flow1.Send(openflow15.FC_ADD)
	verifyFlowInstallAndDelete(t, flow1, NewEmptyElem(), brName, table0.TableId,
		"priority=100,ip,dl_src=11:22:33:44:55:66",
		fmt.Sprintf("output:NXM_NX_REG0[],controller(id=%d)", app.Switch.ctrlID))
}

func testPacketInOut(t *testing.T, ofApp *packetApp, ipv6 bool, reason uint8, controllerV2 bool, dstPort uint16, userData []byte, pause bool) {
	ofSwitch := ofApp.Switch
	srcMAC, _ := net.ParseMAC("11:22:33:44:55:66")
	dstMAC, _ := net.ParseMAC("66:55:44:33:22:11")
	var srcIP net.IP
	var dstIP net.IP
	if ipv6 {
		srcIP = net.ParseIP("2001::1")
		dstIP = net.ParseIP("2002::1")
	} else {
		srcIP = net.ParseIP("1.1.1.2")
		dstIP = net.ParseIP("2.2.2.1")
	}
	// Set PacketIn format.
	pktFmt := uint32(openflow15.OFPUTIL_PACKET_IN_STD)
	if controllerV2 {
		pktFmt = openflow15.OFPUTIL_PACKET_IN_NXT2
	}
	assert.NoError(t, ofSwitch.SetPacketInFormat(pktFmt))

	table0 := ofSwitch.DefaultTable()
	var ethertype uint16
	if ipv6 {
		ethertype = protocol.IPv6_MSG
	} else {
		ethertype = protocol.IPv4_MSG
	}

	flow1 := &Flow{
		Table: table0,
		Match: FlowMatch{
			Priority:  100,
			Ethertype: ethertype,
			MacSa:     &srcMAC,
			MacDa:     &dstMAC,
			IpSa:      &srcIP,
			IpDa:      &dstIP,
			IpProto:   IP_PROTO_TCP,
		},
	}
	rng0 := openflow15.NewNXRange(0, 15)
	rng1 := openflow15.NewNXRange(16, 31)
	rng2 := openflow15.NewNXRange(8, 23)
	act1 := actionToLoadDataToReg(0, 0x1234, rng0)
	act2 := actionToLoadDataToReg(0, 0x5678, rng1)
	act3 := actionToLoadDataToReg(1, 0xaaaa, rng2)
	var expectTunDst net.IP
	if ipv6 {
		expectTunDst = net.ParseIP("2000::10")
	} else {
		expectTunDst = net.ParseIP("10.10.10.10")
	}
	act5 := &SetTunnelDstAction{IP: expectTunDst}
	cxControllerAct := &NXController{Version2: controllerV2, ControllerID: ofSwitch.ctrlID, Reason: reason, UserData: userData, Pause: pause}
	flow1.ApplyActions([]OFAction{act1, act2, act3, act5, cxControllerAct})
	assert.NoError(t, flow1.Send(openflow15.FC_ADD))

	act4 := actionToLoadDataToReg(3, 0xaaaa, rng2)
	packetOut := generateTCPPacketOut(srcMAC, dstMAC, srcIP, dstIP, dstPort, 0, nil, []OFAction{act4})
	if ipv6 {
		assert.NotNil(t, packetOut.IPv6Header)
		assert.Nil(t, packetOut.IPHeader)
		assert.Equal(t, dstIP, packetOut.IPv6Header.NWDst)
	} else {
		assert.NotNil(t, packetOut.IPHeader)
		assert.Nil(t, packetOut.IPv6Header)
	}
	ofSwitch.Send(packetOut.GetMessage())

	var pktIn *PacketIn
	select {
	case pktIn = <-ofApp.pktCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("PacketIn timeout")
	}

	// Delete flow after the packetIn message is received.
	assert.NoError(t, flow1.Delete())

	// Validate packetIn.Reason.
	assert.Equal(t, reason, pktIn.Reason)
	// Validate packetIn.UserData.
	assert.Equal(t, userData, pktIn.UserData)
	// Validate packetIn.Match.
	matchers := pktIn.GetMatches()
	reg0Match := getMatchFieldByRegID(matchers, 0)
	assert.NotNil(t, reg0Match)
	reg0Value, ok := reg0Match.GetValue().(*NXRegister)
	assert.True(t, ok)
	reg0prev := GetUint32ValueWithRange(reg0Value.Data, rng0)
	assert.Equal(t, uint32(0x1234), reg0prev)
	reg0last := GetUint32ValueWithRange(reg0Value.Data, rng1)
	assert.Equal(t, uint32(0x5678), reg0last)
	reg1Match := getMatchFieldByRegID(matchers, 1)
	assert.NotNil(t, reg1Match)
	reg1Value, ok := reg1Match.GetValue().(*NXRegister)
	assert.True(t, ok)
	reg1prev := GetUint32ValueWithRange(reg1Value.Data, rng2)
	assert.Equal(t, uint32(0xaaaa), reg1prev)
	reg2Match := getMatchFieldByRegID(matchers, 2)
	assert.Nil(t, reg2Match)
	var tunDstMatch *MatchField
	if ipv6 {
		tunDstMatch = matchers.GetMatchByName("NXM_NX_TUN_IPV6_DST")
	} else {
		tunDstMatch = matchers.GetMatchByName("NXM_NX_TUN_IPV4_DST")
	}
	assert.NotNil(t, tunDstMatch)
	tunDst := tunDstMatch.GetValue().(net.IP)
	assert.Equal(t, expectTunDst, tunDst)
	ethData := new(protocol.Ethernet)
	err := ethData.UnmarshalBinary(pktIn.Data.(*util.Buffer).Bytes())
	assert.NoError(t, err)
	if ipv6 {
		assert.Equal(t, uint16(protocol.IPv6_MSG), ethData.Ethertype)
		var ipv6Obj protocol.IPv6
		ipv6Bytes, err := ethData.Data.(*protocol.IPv6).MarshalBinary()
		assert.Nil(t, err)
		assert.Nil(t, ipv6Obj.UnmarshalBinary(ipv6Bytes))
		assert.Equal(t, srcIP, ipv6Obj.NWSrc)
		assert.Equal(t, dstIP, ipv6Obj.NWDst)
		assert.Equal(t, uint8(IP_PROTO_TCP), ipv6Obj.NextHeader)
		var tcpObj protocol.TCP
		assert.Nil(t, tcpObj.UnmarshalBinary(ipv6Obj.Data.(*util.Buffer).Bytes()))
		assert.Equal(t, dstPort, tcpObj.PortDst)
	} else {
		assert.Equal(t, dstIP.To4(), ethData.Data.(*protocol.IPv4).NWDst)
	}
	if pause {
		pktIn.Continuation = []byte{7, 8, 9}
		assert.NoError(t, ofSwitch.ResumePacket(pktIn))
	}
}

func actionToLoadDataToReg(regID int, valueData uint32, rng *openflow15.NXRange) OFAction {
	mask := uint32(0)
	if rng != nil {
		mask = ^mask >> (32 - rng.GetNbits()) << rng.GetOfs()
		valueData = valueData << rng.GetOfs()
	}
	f := openflow15.NewRegMatchFieldWithMask(regID, valueData, mask)
	return NewSetFieldAction(f)
}

func getMatchFieldByRegID(matchers *Matchers, regID int) *MatchField {
	xregID := uint8(regID / 2)
	startBit := 4 * (regID % 2)
	f := matchers.GetMatch(openflow15.OXM_CLASS_PACKET_REGS, xregID)
	if f == nil {
		return nil
	}
	dataBytes := f.Value.(*openflow15.ByteArrayField).Data
	data := binary.BigEndian.Uint32(dataBytes[startBit : startBit+4])
	var mask uint32
	if f.HasMask {
		maskBytes, _ := f.Mask.MarshalBinary()
		mask = binary.BigEndian.Uint32(maskBytes[startBit : startBit+4])
	}
	if data == 0 && mask == 0 {
		return nil
	}
	return &MatchField{MatchField: openflow15.NewRegMatchFieldWithMask(regID, data, mask)}
}

func generateTCPPacketOut(srcMAC, dstMAC net.HardwareAddr, srcIP net.IP, dstIP net.IP, dstPort, srcPort uint16, outputPort *uint32, actions []OFAction) *PacketOut {
	var outPort uint32
	if outputPort == nil {
		outPort = openflow15.P_TABLE
	} else {
		outPort = *outputPort
	}
	if dstPort == 0 {
		dstPort = uint16(rand.Uint32())
	}
	if srcPort == 0 {
		srcPort = uint16(rand.Uint32())
	}
	pktOut := GenerateTCPPacket(srcMAC, dstMAC, srcIP, dstIP, dstPort, srcPort, nil)
	pktOut.InPort = openflow15.P_CONTROLLER
	pktOut.OutPort = outPort
	if actions != nil {
		pktOut.Actions = actions
	}
	return pktOut
}

// keeping this in case it is useful later
//
//nolint:unused
func generatePacketOut(srcMAC net.HardwareAddr, dstMAC net.HardwareAddr, srcIP net.IP, dstIP net.IP, outputPort *uint32, actions []OFAction) *PacketOut {
	var outPort uint32
	if outputPort == nil {
		outPort = openflow15.P_TABLE
	} else {
		outPort = *outputPort
	}
	pktOut := GenerateSimpleIPPacket(srcMAC, dstMAC, srcIP, dstIP)
	pktOut.InPort = openflow15.P_CONTROLLER
	pktOut.OutPort = outPort
	if actions != nil {
		pktOut.Actions = actions
	}
	return pktOut
}
