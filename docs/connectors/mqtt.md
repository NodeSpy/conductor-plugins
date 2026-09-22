# `mqtt` connector

MQTT as a connector: **subscribe** to topic filters on a broker as a live source
(one event per received message) and **publish** to a topic (verb). Built on
[eclipse/paho.mqtt.golang](https://github.com/eclipse/paho.mqtt.golang) (MQTT
3.1.1 over tcp/ssl/ws/wss, pure Go). The natural way to react to a homelab's
MQTT bus — Home Assistant, Zigbee2MQTT, sensors, Z-Wave bridges.

- **Kind:** connector (verb **and** source)
- **Source:** [`connectors/mqtt/main.go`](../../connectors/mqtt/main.go)
- **Provides:** `mqtt`
- **Capabilities:** no fixed egress — the broker host is yours; narrow it with `network:`.

```yaml
connectors:
  bus:
    use: mqtt
    broker: tcp://mqtt.local:1883
    username: ${MQTT_USER}
    password: ${MQTT_PASS}
    subscribe: ["home/#", "zigbee2mqtt/+/availability"]
    network: ["mqtt.local:1883"]
```

## Setup

You'll end up with a broker URL and (optionally) credentials.

**Prerequisites:** a reachable MQTT broker (Mosquitto, EMQX, HiveMQ, or the one
Home Assistant runs). Nothing to install on conductor's side — the connection is
outbound.

1. Note the broker's address and scheme: `tcp://host:1883` (plain),
   `ssl://host:8883` (TLS), or `ws://` / `wss://` for WebSocket transports. A
   bare `host:port` is treated as `tcp://`.
2. If the broker requires auth, get a username/password (in Mosquitto, a
   `mosquitto_passwd` entry; in Home Assistant's add-on, a HA user).
3. For a TLS broker with a self-signed cert, set `insecure_skip_verify: true`
   (or, better, make the box trust the broker's CA).
4. List the topic filters to subscribe to under `subscribe:` — `+` matches one
   level, `#` matches the rest (`home/#`, `zigbee2mqtt/+/state`).

**Configure:**

```yaml
connectors:
  bus:
    use: mqtt
    broker: ssl://mqtt.local:8883
    username: ${MQTT_USER}
    password: ${MQTT_PASS}
    qos: 1
    subscribe: ["home/#"]
```

## Connection

| key | type | purpose |
|-----|------|---------|
| `broker` | string (required) | broker URL: `tcp://` \| `ssl://` \| `ws://` \| `wss://` (a bare `host:port` is treated as `tcp://`) |
| `client_id` | string | MQTT client id (default `conductor-mqtt-<instance>`) |
| `username` | string | broker username (optional) |
| `password` | string | broker password (optional) |
| `insecure_skip_verify` | boolean | skip TLS verification for `ssl://` / `wss://` with a self-signed cert |
| `qos` | integer | MQTT QoS `0` \| `1` \| `2` for subscribe and publish (default `0`) |
| `subscribe` | list | topic filters to subscribe to (**StartSource only**); `+` and `#` wildcards |

The connection auto-reconnects (paho), and re-subscribes on every reconnect.

## Source events

Trigger with `on: <name>.<event>`. One event:

| event | fires when | context fields (filter / template) |
|-------|-----------|-------------------------------------|
| `message` | a message arrives on a subscribed topic filter | `topic`, `payload`, `qos`, `retained` |

`retained` is true when the message is the broker's stored last value (delivered
on subscribe), not a live publish.

### Filtering

A trigger's `filter:` matches the published fields. A value must match; a list
matches any of its values; prefix `not_` to negate; `expr:`/`not_expr:` take an
expression; a top-level array of objects is OR. `payload` is text, so parse it
in a step (e.g. the `jq` engine) rather than filtering on structure.

```yaml
triggers:
  - on: bus.message
    filter:
      topics: ["zigbee2mqtt/front_door/contact"]   # any of these exact topics
      not_retained: true                            # ignore the retained snapshot on connect
    steps:
      - use: jq
        code: '{open: (.payload | fromjson | .contact | not)}'
```

## Verbs

- **`publish`** — publish a message to a topic. `topic`* (no wildcards), `payload` (alias `message`), `qos` (`0`|`1`|`2`, default: the connection's `qos`), `retain` (set the retained flag so the broker keeps this as the topic's last value). → `ok`.

```yaml
steps:
  - uses: bus.publish
    options: { topic: "home/office/lamp/set", payload: "ON", qos: 1 }
```

## Capabilities & security

No fixed egress is declared (the broker is operator-specific) — narrow it per
instance with `network:` (e.g. `["mqtt.local:1883"]`); it can never be widened
past the declaration. Spawns nothing. Provide a broker user scoped to just the
topics this instance needs.
