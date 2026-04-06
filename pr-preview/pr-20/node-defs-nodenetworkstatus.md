# Untitled object in Node Schema

```txt
node.json#/$defs/NodeNetworkStatus
```

NodeNetworkStatus reports the overlay-network state of a Node.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [node.json\*](../../schemas/node.json "open original schema") |

## NodeNetworkStatus Type

`object` ([Details](node-defs-nodenetworkstatus.md))

# NodeNetworkStatus Properties

| Property                  | Type      | Required | Nullable       | Defined by                                                                                                            |
| :------------------------ | :-------- | :------- | :------------- | :-------------------------------------------------------------------------------------------------------------------- |
| [ip](#ip)                 | `string`  | Optional | cannot be null | [Node](node-defs-nodenetworkstatus-properties-ip.md "node.json#/$defs/NodeNetworkStatus/properties/ip")               |
| [dnsName](#dnsname)       | `string`  | Optional | cannot be null | [Node](node-defs-nodenetworkstatus-properties-dnsname.md "node.json#/$defs/NodeNetworkStatus/properties/dnsName")     |
| [mode](#mode)             | `string`  | Optional | cannot be null | [Node](node-defs-networkmode.md "node.json#/$defs/NodeNetworkStatus/properties/mode")                                 |
| [agentPort](#agentport)   | `integer` | Optional | cannot be null | [Node](node-defs-nodenetworkstatus-properties-agentport.md "node.json#/$defs/NodeNetworkStatus/properties/agentPort") |
| [throughput](#throughput) | `object`  | Optional | cannot be null | [Node](node-defs-nodethroughput.md "node.json#/$defs/NodeNetworkStatus/properties/throughput")                        |

## ip

IP is the Headscale-assigned overlay IP (e.g. "100.64.0.5").

`ip`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus-properties-ip.md "node.json#/$defs/NodeNetworkStatus/properties/ip")

### ip Type

`string`

## dnsName

DNSName is the MagicDNS FQDN for service discovery.

`dnsName`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus-properties-dnsname.md "node.json#/$defs/NodeNetworkStatus/properties/dnsName")

### dnsName Type

`string`

## mode

Connectivity mode reported by the Tailscale/Headscale overlay network.

`mode`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-networkmode.md "node.json#/$defs/NodeNetworkStatus/properties/mode")

### mode Type

`string`

### mode Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value      | Explanation |
| :--------- | :---------- |
| `"Direct"` |             |
| `"DERP"`   |             |

## agentPort

AgentPort is the TCP port the Agent's HTTP server listens on.
Used by caractl to construct the Agent address for port-forward tunnels.

`agentPort`

* is optional

* Type: `integer`

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus-properties-agentport.md "node.json#/$defs/NodeNetworkStatus/properties/agentPort")

### agentPort Type

`integer`

## throughput

NodeThroughput holds the last measured upload/download speeds of a Node.

`throughput`

* is optional

* Type: `object` ([Details](node-defs-nodethroughput.md))

* cannot be null

* defined in: [Node](node-defs-nodethroughput.md "node.json#/$defs/NodeNetworkStatus/properties/throughput")

### throughput Type

`object` ([Details](node-defs-nodethroughput.md))
