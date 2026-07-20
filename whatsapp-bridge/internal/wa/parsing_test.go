package wa

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestExtractTextContent(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"nil", nil, ""},
		{"conversation", &waE2E.Message{Conversation: proto.String("hola")}, "hola"},
		{
			"extended",
			&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("mundo")}},
			"mundo",
		},
		{
			"image caption",
			&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("una foto")}},
			"una foto",
		},
		{
			"ephemeral unwrap",
			&waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{Conversation: proto.String("efímero")},
			}},
			"efímero",
		},
		{"empty", &waE2E.Message{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractTextContent(tc.msg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractTextContentSpecialTypes cubre las ramas sin archivo descargable
// (ubicación, contactos, encuesta, invitación) y los wrappers/captions restantes:
// son el texto que termina persistido en la DB y leído por list_messages.
func TestExtractTextContentSpecialTypes(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{
			// La ubicación no tiene media: el contenido legible son coords + nombre + dirección.
			"ubicación con nombre y dirección",
			&waE2E.Message{LocationMessage: &waE2E.LocationMessage{
				DegreesLatitude:  proto.Float64(4.5),
				DegreesLongitude: proto.Float64(-74.25),
				Name:             proto.String("Oficina"),
				Address:          proto.String("Calle 1 # 2-3"),
			}},
			"📍 Ubicación: 4.500000, -74.250000 — Oficina (Calle 1 # 2-3)",
		},
		{
			// Sin nombre ni dirección solo quedan las coords (ramas opcionales apagadas).
			"ubicación solo coordenadas",
			&waE2E.Message{LocationMessage: &waE2E.LocationMessage{
				DegreesLatitude:  proto.Float64(4.5),
				DegreesLongitude: proto.Float64(-74.25),
			}},
			"📍 Ubicación: 4.500000, -74.250000",
		},
		{
			"ubicación en vivo con caption",
			&waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
				DegreesLatitude:  proto.Float64(4.5),
				DegreesLongitude: proto.Float64(-74.25),
				Caption:          proto.String("voy en camino"),
			}},
			"📍 Ubicación en vivo: 4.500000, -74.250000 — voy en camino",
		},
		{
			// El teléfono se parsea de la línea TEL del vCard, no de un campo dedicado.
			"contacto con teléfono del vCard",
			&waE2E.Message{ContactMessage: &waE2E.ContactMessage{
				DisplayName: proto.String("Ana"),
				Vcard:       proto.String("BEGIN:VCARD\nTEL;type=CELL:+573001112233\nEND:VCARD"),
			}},
			"👤 Contacto: Ana · +573001112233",
		},
		{
			"contacto sin teléfono en el vCard",
			&waE2E.Message{ContactMessage: &waE2E.ContactMessage{
				DisplayName: proto.String("Ana"),
				Vcard:       proto.String("BEGIN:VCARD\nFN:Ana\nEND:VCARD"),
			}},
			"👤 Contacto: Ana",
		},
		{
			// Mezcla con y sin teléfono: solo quien tiene TEL lleva el paréntesis.
			"array de contactos",
			&waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{
				Contacts: []*waE2E.ContactMessage{
					{DisplayName: proto.String("Ana"), Vcard: proto.String("TEL:+111")},
					{DisplayName: proto.String("Beto")},
				},
			}},
			"👤 2 contactos: Ana (+111), Beto",
		},
		{
			// Array vacío: se reporta el conteo sin la lista (rama len(names) == 0).
			"array de contactos vacío",
			&waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{}},
			"👤 0 contactos",
		},
		{
			// El texto legible de una encuesta es su pregunta.
			"encuesta devuelve la pregunta",
			&waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{
				Name: proto.String("¿Almuerzo?"),
			}},
			"¿Almuerzo?",
		},
		{
			"invitación a grupo con nombre",
			&waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{
				GroupName: proto.String("Mi Grupo"),
			}},
			"📨 Invitación a grupo: Mi Grupo",
		},
		{
			// Sin nombre de grupo cae al JID (fallback para invitaciones sin metadata).
			"invitación a grupo sin nombre cae al JID",
			&waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{
				GroupJID: proto.String("123456789@g.us"),
			}},
			"📨 Invitación a grupo: 123456789@g.us",
		},
		{
			// Los wrappers se desenrollan igual que en ExtractMediaInfo: sin esto el
			// texto de un view-once se perdería.
			"view-once se desenrolla",
			&waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{Conversation: proto.String("secreto")},
			}},
			"secreto",
		},
		{
			"documento con caption se desenrolla",
			&waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
					Caption: proto.String("informe adjunto"),
				}},
			}},
			"informe adjunto",
		},
		{
			"caption de video",
			&waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("mira esto")}},
			"mira esto",
		},
		{
			"caption de documento",
			&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("el pdf")}},
			"el pdf",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractTextContent(tc.msg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractEditedText cubre las tres formas en que puede llegar el plaintext de
// un edit descifrado (contenido directo, ProtocolMessage.EditedMessage y el wrapper
// EditedMessage) — el handler de edits depende de que las tres resuelvan.
func TestExtractEditedText(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"nil", nil, ""},
		{
			"contenido directo",
			&waE2E.Message{Conversation: proto.String("texto nuevo")},
			"texto nuevo",
		},
		{
			// Forma anidada: el texto viene dentro de ProtocolMessage.EditedMessage.
			"anidado en ProtocolMessage.EditedMessage",
			&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				EditedMessage: &waE2E.Message{Conversation: proto.String("editado vía protocol")},
			}},
			"editado vía protocol",
		},
		{
			// Forma wrapper: Message.EditedMessage (FutureProofMessage) envuelve el mensaje.
			"wrapper EditedMessage",
			&waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{Conversation: proto.String("editado vía wrapper")},
			}},
			"editado vía wrapper",
		},
		{"sin texto en ninguna forma", &waE2E.Message{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractEditedText(tc.msg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractMediaInfo cubre el parsing de media descargable: cada tipo debe
// propagar url/direct_path/claves/hashes (el direct_path es el dato CRÍTICO:
// whatsmeow.Download solo usa GetDirectPath y sin él la descarga mms3 da 403).
// Los filenames llevan timestamp de time.Now(), por eso se chequean por prefijo/sufijo.
func TestExtractMediaInfo(t *testing.T) {
	// Metadata compartida por todos los casos de media real.
	const (
		wantURL        = "https://mmg.whatsapp.net/v/t62.7118-24/file.enc?ccb=11-4"
		wantDirectPath = "/v/t62.7118-24/file.enc"
	)
	var (
		wantKey    = []byte{1, 2, 3}
		wantSHA    = []byte{4, 5, 6}
		wantEncSHA = []byte{7, 8, 9}
	)
	const wantLength = uint64(2048)

	imageMsg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		URL: proto.String(wantURL), DirectPath: proto.String(wantDirectPath),
		MediaKey: wantKey, FileSHA256: wantSHA, FileEncSHA256: wantEncSHA,
		FileLength: proto.Uint64(wantLength),
	}}

	tests := []struct {
		name       string
		msg        *waE2E.Message
		wantType   string
		wantPrefix string // prefijo del filename generado
		wantSuffix string // sufijo (extensión); "" = no se chequea
		hasMedia   bool   // true = debe propagar url/direct_path/claves/hashes/length
	}{
		{"nil", nil, "", "", "", false},
		{"sin media todo vacío", &waE2E.Message{Conversation: proto.String("hola")}, "", "", "", false},
		{"imagen", imageMsg, "image", "image_", ".jpg", true},
		{
			"video",
			&waE2E.Message{VideoMessage: &waE2E.VideoMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantDirectPath),
				MediaKey: wantKey, FileSHA256: wantSHA, FileEncSHA256: wantEncSHA,
				FileLength: proto.Uint64(wantLength),
			}},
			"video", "video_", ".mp4", true,
		},
		{
			"audio",
			&waE2E.Message{AudioMessage: &waE2E.AudioMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantDirectPath),
				MediaKey: wantKey, FileSHA256: wantSHA, FileEncSHA256: wantEncSHA,
				FileLength: proto.Uint64(wantLength),
			}},
			"audio", "audio_", ".ogg", true,
		},
		{
			// El documento conserva su nombre original si viene en el protobuf.
			"documento con filename propio",
			&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				FileName: proto.String("informe.pdf"),
				URL:      proto.String(wantURL), DirectPath: proto.String(wantDirectPath),
				MediaKey: wantKey, FileSHA256: wantSHA, FileEncSHA256: wantEncSHA,
				FileLength: proto.Uint64(wantLength),
			}},
			"document", "informe.pdf", ".pdf", true,
		},
		{
			// Sin FileName cae al fallback generado con timestamp (no queda vacío).
			"documento sin filename cae al fallback",
			&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantDirectPath),
				MediaKey: wantKey, FileSHA256: wantSHA, FileEncSHA256: wantEncSHA,
				FileLength: proto.Uint64(wantLength),
			}},
			"document", "document_", "", true,
		},
		{
			// Stickers estáticos y animados llegan como .webp (whatsmeow los baja como imagen).
			"sticker",
			&waE2E.Message{StickerMessage: &waE2E.StickerMessage{
				URL: proto.String(wantURL), DirectPath: proto.String(wantDirectPath),
				MediaKey: wantKey, FileSHA256: wantSHA, FileEncSHA256: wantEncSHA,
				FileLength: proto.Uint64(wantLength),
			}},
			"sticker", "sticker_", ".webp", true,
		},
		{
			// Los wrappers envuelven el mensaje real: sin desenrollarlos la media
			// de un mensaje temporal se perdería.
			"wrapper ephemeral se desenrolla",
			&waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: imageMsg}},
			"image", "image_", ".jpg", true,
		},
		{
			"wrapper view-once se desenrolla",
			&waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{Message: imageMsg}},
			"image", "image_", ".jpg", true,
		},
		{
			"wrapper documento+caption se desenrolla",
			&waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: imageMsg}},
			"image", "image_", ".jpg", true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotFilename, gotURL, gotDirectPath, gotKey, gotSHA, gotEncSHA, gotLen := ExtractMediaInfo(tc.msg)
			if gotType != tc.wantType {
				t.Errorf("mediaType: got %q, want %q", gotType, tc.wantType)
			}
			if !strings.HasPrefix(gotFilename, tc.wantPrefix) || !strings.HasSuffix(gotFilename, tc.wantSuffix) {
				t.Errorf("filename: got %q, want prefijo %q y sufijo %q", gotFilename, tc.wantPrefix, tc.wantSuffix)
			}
			if tc.hasMedia {
				if gotURL != wantURL {
					t.Errorf("url: got %q, want %q", gotURL, wantURL)
				}
				if gotDirectPath != wantDirectPath {
					t.Errorf("directPath: got %q, want %q", gotDirectPath, wantDirectPath)
				}
				if !bytes.Equal(gotKey, wantKey) || !bytes.Equal(gotSHA, wantSHA) || !bytes.Equal(gotEncSHA, wantEncSHA) {
					t.Errorf("claves/hashes no propagados: key=%v sha=%v encSha=%v", gotKey, gotSHA, gotEncSHA)
				}
				if gotLen != wantLength {
					t.Errorf("fileLength: got %d, want %d", gotLen, wantLength)
				}
			} else {
				if gotURL != "" || gotDirectPath != "" || gotKey != nil || gotSHA != nil || gotEncSHA != nil || gotLen != 0 {
					t.Errorf("sin media debería devolver todo vacío, got url=%q directPath=%q key=%v len=%d",
						gotURL, gotDirectPath, gotKey, gotLen)
				}
			}
		})
	}
}

// TestExtractMediaInfoPoll: una encuesta NO es media descargable, pero se persiste con
// media_type="poll" y sus opciones serializadas como JSON en filename — sin ese JSON los
// votos entrantes (que llegan como hashes) no se pueden mapear a nombres legibles.
func TestExtractMediaInfoPoll(t *testing.T) {
	msg := &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{
		Name: proto.String("¿Almuerzo?"),
		Options: []*waE2E.PollCreationMessage_Option{
			{OptionName: proto.String("Pizza")},
			{OptionName: proto.String("Sushi")},
		},
	}}
	mediaType, filename, url, directPath, mediaKey, _, _, fileLength := ExtractMediaInfo(msg)
	if mediaType != "poll" {
		t.Errorf("mediaType: got %q, want %q", mediaType, "poll")
	}
	if filename != `["Pizza","Sushi"]` {
		t.Errorf("opciones serializadas: got %q, want %q", filename, `["Pizza","Sushi"]`)
	}
	// No hay nada que descargar: los campos de media deben quedar vacíos.
	if url != "" || directPath != "" || mediaKey != nil || fileLength != 0 {
		t.Errorf("un poll no es media descargable: url=%q directPath=%q key=%v len=%d",
			url, directPath, mediaKey, fileLength)
	}
}

// TestExtractMediaInfoGroupInvite: la invitación se persiste con media_type="group_invite"
// y los datos para unirse (group_jid/code/expiration) como JSON en filename; el inviter
// no viaja aquí (es el sender del mensaje).
func TestExtractMediaInfoGroupInvite(t *testing.T) {
	msg := &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{
		GroupJID:         proto.String("123456789@g.us"),
		InviteCode:       proto.String("ABCDefgh1234"),
		InviteExpiration: proto.Int64(1751371200),
		GroupName:        proto.String("Mi Grupo"),
	}}
	mediaType, filename, url, directPath, mediaKey, _, _, fileLength := ExtractMediaInfo(msg)
	if mediaType != "group_invite" {
		t.Fatalf("mediaType: got %q, want %q", mediaType, "group_invite")
	}
	var data struct {
		GroupJID   string `json:"group_jid"`
		Code       string `json:"code"`
		Expiration int64  `json:"expiration"`
		GroupName  string `json:"group_name"`
	}
	if err := json.Unmarshal([]byte(filename), &data); err != nil {
		t.Fatalf("el filename debería ser JSON válido: %v (got %q)", err, filename)
	}
	if data.GroupJID != "123456789@g.us" {
		t.Errorf("group_jid: got %q, want %q", data.GroupJID, "123456789@g.us")
	}
	if data.Code != "ABCDefgh1234" {
		t.Errorf("code: got %q, want %q", data.Code, "ABCDefgh1234")
	}
	if data.Expiration != 1751371200 {
		t.Errorf("expiration: got %d, want %d", data.Expiration, 1751371200)
	}
	if data.GroupName != "Mi Grupo" {
		t.Errorf("group_name: got %q, want %q", data.GroupName, "Mi Grupo")
	}
	if url != "" || directPath != "" || mediaKey != nil || fileLength != 0 {
		t.Errorf("una invitación no es media descargable: url=%q directPath=%q key=%v len=%d",
			url, directPath, mediaKey, fileLength)
	}
}

func TestVcardPhone(t *testing.T) {
	tests := []struct {
		name  string
		vcard string
		want  string
	}{
		{"vacío", "", ""},
		{"tel simple", "BEGIN:VCARD\nTEL:+5491122334455\nEND:VCARD", "+5491122334455"},
		{"tel con params", "TEL;type=CELL;waid=549112233:+54 9 11 2233", "+54 9 11 2233"},
		{"sin tel", "BEGIN:VCARD\nFN:Juan\nEND:VCARD", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vcardPhone(tc.vcard); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveMentions(t *testing.T) {
	t.Run("número explícito a JID", func(t *testing.T) {
		got := ResolveMentions("", []string{"5491122334455"})
		want := []string{"5491122334455@s.whatsapp.net"}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("auto-detección en texto", func(t *testing.T) {
		got := ResolveMentions("hola @5491122334455 qué tal", nil)
		if len(got) != 1 || got[0] != "5491122334455@s.whatsapp.net" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("dedup conservando orden", func(t *testing.T) {
		got := ResolveMentions("@5491122334455", []string{"5491122334455"})
		if len(got) != 1 {
			t.Errorf("debería deduplicar, got %v", got)
		}
	})
	t.Run("JID explícito pasa tal cual", func(t *testing.T) {
		got := ResolveMentions("", []string{"123-456@g.us"})
		if len(got) != 1 || got[0] != "123-456@g.us" {
			t.Errorf("got %v", got)
		}
	})
}

func TestParseParticipantJIDs(t *testing.T) {
	t.Run("número a JID", func(t *testing.T) {
		jids, err := ParseParticipantJIDs([]string{"5491122334455"})
		if err != nil {
			t.Fatalf("no debería fallar: %v", err)
		}
		if len(jids) != 1 || jids[0].String() != "5491122334455@s.whatsapp.net" {
			t.Errorf("got %v", jids)
		}
	})
	t.Run("vacíos se saltan", func(t *testing.T) {
		jids, err := ParseParticipantJIDs([]string{"", "  ", "5491122334455"})
		if err != nil {
			t.Fatalf("no debería fallar: %v", err)
		}
		if len(jids) != 1 {
			t.Errorf("los vacíos deberían saltarse, got %v", jids)
		}
	})
}

func TestExtractDirectPathFromURL(t *testing.T) {
	url := "https://mmg.whatsapp.net/v/t62.7118-24/13812002_n.enc?ccb=11-4&oh=abc"
	got := ExtractDirectPathFromURL(url)
	if got != "/v/t62.7118-24/13812002_n.enc" {
		t.Errorf("got %q", got)
	}
	// Sin ".net/": devuelve la URL original.
	if got := ExtractDirectPathFromURL("no-url"); got != "no-url" {
		t.Errorf("fallback got %q", got)
	}
}
