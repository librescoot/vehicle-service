package hardware

import "testing"

// A /proc/net/route sample with the two service routes to the DBC (192.168.7.2,
// little-endian 0207A8C0) at /32, alongside routes that must not match.
const procRouteBoth = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
usb0	0207A8C0	0209A8C0	0003	0	0	50	FFFFFFFF	0	0	0
ppp0	0207A8C0	0208A8C0	0003	0	0	200	FFFFFFFF	0	0	0
usb0	0007A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
eth1	00000000	0100A8C0	0003	0	0	25	00000000	0	0	0
`

func TestParseDbcLinks(t *testing.T) {
	tests := []struct {
		name       string
		data       string
		wantActive string
		wantUSB    bool
		wantPPP    bool
	}{
		{
			name:       "both links up prefers USB",
			data:       procRouteBoth,
			wantActive: "usb0",
			wantUSB:    true,
			wantPPP:    true,
		},
		{
			name: "USB down falls back to PPP",
			data: `usb0	0007A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
ppp0	0207A8C0	0208A8C0	0003	0	0	200	FFFFFFFF	0	0	0
`,
			wantActive: "ppp0",
			wantPPP:    true,
		},
		{
			name: "USB up, PPP never came up",
			data: `usb0	0207A8C0	0209A8C0	0003	0	0	50	FFFFFFFF	0	0	0
ppp0	0008A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`,
			wantActive: "usb0",
			wantUSB:    true,
		},
		{
			name:       "neither route",
			data:       "eth1	00000000	0100A8C0	0003	0	0	25	00000000	0	0	0\n",
			wantActive: "",
		},
		{
			name:       "header only",
			data:       "Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT\n",
			wantActive: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDbcLinks(tt.data)
			if got.Active != tt.wantActive || got.USB != tt.wantUSB || got.PPP != tt.wantPPP {
				t.Fatalf("parseDbcLinks() = %+v, want active %q usb %v ppp %v",
					got, tt.wantActive, tt.wantUSB, tt.wantPPP)
			}
		})
	}
}
