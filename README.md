# WhatsApp Plus MCP

**Tu WhatsApp personal como servidor MCP: 66 herramientas, login por QR autogestionado y cero pasos manuales.**

Conecta tu cuenta personal de WhatsApp a Claude (o a cualquier agente compatible con MCP) para leer, buscar y enviar mensajes, administrar grupos, manejar multimedia y más — todo a través de tu propia cuenta, ejecutándose 100 % en tu máquina. Nada pasa por servidores de terceros.

> Fork profesionalizado de [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) (~12 tools originales → **66 tools**), con hardening de seguridad, arquitectura modular, suite de tests, supervisor de procesos y login plug-and-play.

---

## ✨ Lo que lo hace diferente

- **Login autogestionado**: pide "conéctame a WhatsApp" y el plugin hace todo — lanza su propio bridge, valida si ya hay una sesión utilizable (nunca duplica conexiones), y solo si hace falta abre el **código QR en tu visor de imágenes** (instantáneo, se refresca solo con cada rotación) y también lo muestra en la conversación. Escaneas y listo.
- **66 herramientas MCP**: mensajes (enviar, responder, editar, borrar, reaccionar, destacar), búsqueda e historial, grupos (crear, administrar, invitaciones), multimedia (imágenes, notas de voz, documentos, descarga), presencia, encuestas, contactos, estados de chat y gestión de sesión.
- **Supervisor integrado**: el servidor MCP administra el ciclo de vida del bridge Go (adopta uno sano, compila el binario si falta, recicla sesiones zombie). El usuario no toca ninguna terminal.
- **Seguridad por diseño**: API solo en loopback con token de autenticación, validación anti-exfiltración de rutas de archivos, datos siempre en tu máquina.
- **MCP estándar y transversal**: funciona como plugin de Claude Code **y** como servidor MCP clásico en Claude Desktop, Cursor, Gemini CLI, Codex CLI o cualquier cliente MCP.

## 🏗️ Arquitectura

Dos procesos cooperando en tu máquina:

```
┌──────────────────┐   MCP (stdio)   ┌───────────────────┐   REST loopback    ┌────────────────┐
│ Agente (Claude,  │ ◄─────────────► │ Servidor MCP      │ ◄────────────────► │ Bridge Go      │ ◄──► WhatsApp
│ Cursor, Gemini…) │                 │ (Python/FastMCP)  │   127.0.0.1:8080   │ (whatsmeow)    │
└──────────────────┘                 └───────────────────┘   + X-Auth-Token   └────────────────┘
                                        │ lecturas directas                      │ sesión + historial
                                        ▼                                        ▼
                                     SQLite (messages.db)                    SQLite (whatsapp.db)
```

- **Bridge Go** (`whatsapp-bridge/`): conecta con la API multidevice de WhatsApp Web vía [whatsmeow](https://github.com/tulir/whatsmeow), maneja la autenticación QR, persiste mensajes/chats en SQLite y expone una REST API autenticada solo en loopback.
- **Servidor MCP Python** (`whatsapp-mcp-server/`): expone las 66 tools. Las **lecturas** consultan SQLite directamente; las **acciones** van al bridge por HTTP. Además **supervisa** al bridge: lo lanza, lo adopta o lo recicla según haga falta.

## 📋 Requisitos

| Requisito | Para qué | Instalación |
|-----------|----------|-------------|
| [uv](https://docs.astral.sh/uv/) | Ejecutar el servidor MCP Python | `brew install uv` |
| [Go](https://go.dev/dl/) ≥ 1.25 *(opcional)* | Solo como fallback: el bridge se descarga precompilado desde GitHub Releases (verificado SHA256); Go únicamente hace falta si no hay release para tu plataforma | `brew install go` |
| [FFmpeg](https://ffmpeg.org/) *(opcional)* | Convertir audio a notas de voz (`send_audio_message`) | `brew install ffmpeg` |

## 🚀 Instalación

### Opción A — Plugin de Claude Code (recomendada)

Este repositorio es a la vez el **plugin** y su **marketplace**. Desde Claude Code:

```
/plugin marketplace add pachperdev/whatsapp-plus-mcp
/plugin install whatsapp-plus@pachperdev
```

Para preconfigurar el marketplace en un proyecto (todos lo reciben al abrir el repo), agrega a su `.claude/settings.json`:

```json
{
  "extraKnownMarketplaces": {
    "pachperdev": {
      "source": { "source": "github", "repo": "pachperdev/whatsapp-plus-mcp" }
    }
  },
  "enabledPlugins": { "whatsapp-plus@pachperdev": true }
}
```

En modo plugin, **todos los datos viven en `~/.whatsapp-mcp/`** (sesión, historial, binario, logs) — los updates del plugin nunca tocan tu sesión.

### Opción B — Servidor MCP estándar (cualquier agente)

Clona el repo y registra el servidor en tu cliente MCP. El comando es siempre el mismo:

```bash
git clone https://github.com/pachperdev/whatsapp-plus-mcp.git
```

**Claude Code**: el repo ya incluye `.mcp.json` — basta abrir el proyecto. O manual:

```bash
claude mcp add whatsapp-plus -- uv --directory /ruta/al/repo/whatsapp-mcp-server run main.py
```

**Claude Desktop** (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "whatsapp-plus": {
      "command": "uv",
      "args": ["--directory", "/ruta/al/repo/whatsapp-mcp-server", "run", "main.py"]
    }
  }
}
```

**Cursor** (`~/.cursor/mcp.json`), **Gemini CLI** (`~/.gemini/settings.json` → `mcpServers`) y **Codex CLI** (`~/.codex/config.toml` → `[mcp_servers.whatsapp-plus]`) usan la misma forma: comando `uv` con argumentos `--directory /ruta/al/repo/whatsapp-mcp-server run main.py`.

## 🔐 Primer uso: conectar tu WhatsApp

Simplemente pídeselo a tu agente:

> *"Conéctame a WhatsApp"*

El agente llama a `login_with_qr` y el plugin hace el resto:

1. Verifica si hay un bridge corriendo con **sesión válida** → la reutiliza (sin QR).
2. Si no existe el binario del bridge, **descarga el precompilado** del último release (verificación SHA256; usa tu token de `gh` para el repo privado) o, como fallback, lo compila con Go.
3. Si hace falta login, abre el **QR en tu visor de imágenes** (canal principal: instantáneo y auto-refrescado en cada rotación) y también lo muestra en la conversación.
4. Escaneas desde WhatsApp → **Ajustes → Dispositivos vinculados → Vincular un dispositivo**.

La sesión persiste ~20 días; después WhatsApp puede pedir re-vincular (mismo flujo, un escaneo).

## 🧰 Las 66 herramientas

| Área | Herramientas |
|------|--------------|
| **Sesión** | `login_with_qr` (QR inline + visor), `get_status`, `logout`, `shutdown_bridge` |
| **Leer/buscar** | `list_messages`, `list_chats`, `search_contacts`, `list_all_contacts`, `get_message_context`, `get_unread_chats`, `get_last_interaction`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `list_groups`, `refresh_contacts` |
| **Enviar** | `send_message` (con reply y @menciones), `send_file`, `send_audio_message` (nota de voz), `send_poll`, `vote_poll`, `send_typing` |
| **Mensajes** | `react_to_message`, `edit_message`, `delete_message`, `star_message`, `mark_as_read`, `download_media` |
| **Chats** | `mute_chat`, `pin_chat`, `archive_chat`, `delete_chat`, `mark_chat`, `get_chat_settings`, `set_disappearing_messages`, `request_more_history` |
| **Grupos** | `create_group`, `update_group_participants`, `get_group_participants`, `get_group_invite_link`, `reset_group_invite_link`, `join_group`, `leave_group`, `set_group_name/topic/description/announce/locked/photo`, aprobación de ingreso, solicitudes pendientes, invitaciones |
| **Identidad/presencia** | `check_whatsapp`, `get_user_info`, `get_user_devices`, `get_profile_picture`, `get_business_profile`, `block_contact`, `unblock_contact`, `set/subscribe/get_presence`, `set_status_message`, `set_default_disappearing` |

Cada tool lleva anotaciones MCP (`readOnly`, `destructive`, `idempotent`) para que el cliente pida confirmación solo cuando corresponde.

## 🛡️ Seguridad

- **Solo loopback**: la REST API del bridge escucha únicamente en `127.0.0.1` (validado al arrancar) y **toda** ruta exige el header `X-Auth-Token` (token aleatorio generado por el bridge, comparación en tiempo constante, fail-closed).
- **Anti-exfiltración**: el envío de archivos valida rutas por inodo — rechaza rutas ocultas y los secretos del propio store; `WHATSAPP_MEDIA_ALLOWED_DIRS` permite confinar los envíos a una lista blanca.
- **Tus datos, tu máquina**: mensajes y sesión se guardan en SQLite local. Nada sale hacia servicios de terceros; los mensajes solo llegan al agente cuando una tool los consulta.
- Como todo MCP con acceso a datos privados, aplica el criterio de la [trifecta letal](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/): sé deliberado con lo que dejas hacer al agente.

## ⚠️ Advertencia importante

Este proyecto usa [whatsmeow](https://github.com/tulir/whatsmeow), un cliente **no oficial**. Su uso viola los Términos de Servicio de WhatsApp y conlleva riesgo de bloqueo de la cuenta. Reglas de oro para minimizar riesgo:

- **Nada de envíos masivos** ni mensajes idénticos repetidos (dispara el ban código 104).
- Uso **reactivo** (responder a mensajes entrantes) es de bajo riesgo; el outreach frío/proactivo es de alto riesgo.
- Una cuenta = una sesión = una IP. El supervisor garantiza que nunca haya conexiones duplicadas.
- El bridge detecta bans temporales y **pausa automáticamente los envíos** hasta que expiren.

## ⚙️ Configuración (variables de entorno)

| Variable | Default | Descripción |
|----------|---------|-------------|
| `WHATSAPP_PLUGIN_MODE` | — | `1` = datos en `~/.whatsapp-mcp/` (lo setea el plugin) |
| `WHATSAPP_STORE_DIR` | `<repo>/whatsapp-bridge/store` | Directorio del store (sesión, mensajes, media) |
| `WHATSAPP_BRIDGE_BIN` | `<repo>/whatsapp-bridge/whatsapp-bridge` | Binario del bridge (si falta: se descarga del release o se auto-compila) |
| `WHATSAPP_RELEASE_REPO` | `pachperdev/whatsapp-plus-mcp` | Repo GitHub del que se descargan los binarios precompilados |
| `WHATSAPP_BRIDGE_ADDR` | `127.0.0.1:8080` | Dirección del bridge (validada como loopback) |
| `WHATSAPP_MEDIA_ALLOWED_DIRS` | — | Lista blanca de directorios para `send_file` |
| `WHATSAPP_MESSAGES_DB` / `WHATSAPP_SESSION_DB` / `WHATSAPP_BRIDGE_TOKEN_FILE` / `WHATSAPP_BRIDGE_LOG` | derivados del store | Overrides finos |

## 🧑‍💻 Desarrollo

```bash
# Bridge Go
cd whatsapp-bridge
go build ./... && go vet ./... && go test ./...
go build -o whatsapp-bridge .        # binario (sin CGO: cross-compila a darwin/linux/windows)

# Servidor MCP Python
cd whatsapp-mcp-server
uv run pytest -q && uvx ruff check .
uv run main.py                        # correr el server (modo repo)
```

La guía de arquitectura para agentes de código vive en [`CLAUDE.md`](CLAUDE.md) (patrón canónico para agregar tools, modelo de seguridad, esquema de la base, gotchas de whatsmeow). El historial de hitos está en [`CHANGELOG.md`](CHANGELOG.md).

## 🩺 Solución de problemas

- **"no existe el binario del bridge..."** → verifica `gh auth login` (la descarga del release lo usa) o instala Go (`brew install go`) como fallback; el supervisor resuelve solo.
- **El QR expiró** → vuelve a pedir la conexión; `login_with_qr` siempre entrega el código vigente.
- **"app state en recuperación"** al destacar/silenciar/archivar → tu teléfono debe estar en línea; reintenta en unos segundos (recuperación automática vía teléfono primario).
- **Logs del bridge** → `~/.whatsapp-mcp/store/bridge.log` (modo plugin) o `<repo>/whatsapp-bridge/store/bridge.log`.

## 🙏 Créditos

- [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) — proyecto original.
- [tulir/whatsmeow](https://github.com/tulir/whatsmeow) — cliente Go de WhatsApp Web multidevice.
- [Model Context Protocol](https://modelcontextprotocol.io) — el estándar que hace esto transversal a cualquier agente.
