package pdf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMalformedInputsDoNotPanic(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"empty":       "",
		"header_only": "%PDF-1.7",
		"huge_objstm_offsets": "%PDF-1.7\n1 0 obj\n<</Type/ObjStm/N 2/First 10>>stream\n1 9223372036854775807 2 3\nendstream\nendobj\n" +
			"2 0 obj\n<</Type/Page/MediaBox[0 0 100 100]>>\nendobj\n",
		"huge_image_dims": "%PDF-1.7\n1 0 obj\n<</Type/Page/MediaBox[0 0 100 100]/Resources<</XObject<</Im 2 0 R>>>>>>\nendobj\n" +
			"2 0 obj\n<</Subtype/Image/Width 99999999999999999/Height 99999999999999999/BitsPerComponent 8/ColorSpace/DeviceRGB/Filter/FlateDecode>>stream\nendstream\nendobj\n",
		"huge_smask_dims": "%PDF-1.7\n1 0 obj\n<</Type/Page/MediaBox[0 0 100 100]/Resources<</XObject<</Im 2 0 R>>>>>>\nendobj\n" +
			"2 0 obj\n<</Subtype/Image/Filter/DCTDecode/SMask 3 0 R>>stream\nendstream\nendobj\n" +
			"3 0 obj\n<</BitsPerComponent 8/Width 99999999999999999/Height 99999999999999999>>stream\nendstream\nendobj\n",
		"page_cycle": "%PDF-1.7\n1 0 obj\n<</Type/Catalog/Pages 2 0 R>>\nendobj\n" +
			"2 0 obj\n<</Type/Pages/Kids[2 0 R 3 0 R]>>\nendobj\n" +
			"3 0 obj\n<</Type/Page/MediaBox[0 0 100 100]>>\nendobj\n",
		"parent_cycle": "%PDF-1.7\n1 0 obj\n<</Type/Catalog/Pages 2 0 R>>\nendobj\n" +
			"2 0 obj\n<</Type/Pages/Kids[3 0 R]>>\nendobj\n" +
			"3 0 obj\n<</Type/Page/Parent 3 0 R/MediaBox[0 0 100 100]>>\nendobj\n",
		"unterminated_string": "%PDF-1.7\n1 0 obj\n<</Type/Page/MediaBox[0 0 100 100]/Contents 2 0 R>>\nendobj\n" +
			"2 0 obj\n<</Length 10>>stream\nBT (abc\nendstream\nendobj\n",
		"stray_delims": "%PDF-1.7\n1 0 obj\n<</Type/Page/MediaBox[0 0 100 100]/Contents 2 0 R>>\nendobj\n" +
			"2 0 obj\n<</Length 10>>stream\nBT )]> Tj <zz TJ\nendstream\nendobj\n",
	}
	for name, content := range cases {
		p := filepath.Join(dir, name+".pdf")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic: %v", name, r)
				}
			}()
			ParseFile(p) // error or nil both acceptable; must not panic
		}()
	}
}
