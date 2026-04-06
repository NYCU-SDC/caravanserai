# Node Schema

```txt
node.json
```



| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                  |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :---------------------------------------------------------- |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [node.json](../../schemas/node.json "open original schema") |

## Node Type

unknown ([Node](node.md))

# Node Definitions

## Definitions group Condition

Reference this group by using

```json
{"$ref":"node.json#/$defs/Condition"}
```

| Property                                  | Type     | Required | Nullable       | Defined by                                                                                                              |
| :---------------------------------------- | :------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------------------- |
| [type](#type)                             | `string` | Required | cannot be null | [Node](node-defs-condition-properties-type.md "node.json#/$defs/Condition/properties/type")                             |
| [status](#status)                         | `string` | Required | cannot be null | [Node](node-defs-condition-properties-status.md "node.json#/$defs/Condition/properties/status")                         |
| [lastHeartbeatTime](#lastheartbeattime)   | `string` | Optional | cannot be null | [Node](node-defs-condition-properties-lastheartbeattime.md "node.json#/$defs/Condition/properties/lastHeartbeatTime")   |
| [lastTransitionTime](#lasttransitiontime) | `string` | Optional | cannot be null | [Node](node-defs-condition-properties-lasttransitiontime.md "node.json#/$defs/Condition/properties/lastTransitionTime") |
| [reason](#reason)                         | `string` | Optional | cannot be null | [Node](node-defs-condition-properties-reason.md "node.json#/$defs/Condition/properties/reason")                         |
| [message](#message)                       | `string` | Optional | cannot be null | [Node](node-defs-condition-properties-message.md "node.json#/$defs/Condition/properties/message")                       |

### type

Type is a machine-readable identifier, e.g. "Ready", "Phase".

`type`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-type.md "node.json#/$defs/Condition/properties/type")

#### type Type

`string`

### status

Status is one of True, False, Unknown.

`status`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-status.md "node.json#/$defs/Condition/properties/status")

#### status Type

`string`

### lastHeartbeatTime

LastHeartbeatTime is when this condition was last sampled.

`lastHeartbeatTime`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-lastheartbeattime.md "node.json#/$defs/Condition/properties/lastHeartbeatTime")

#### lastHeartbeatTime Type

`string`

#### lastHeartbeatTime Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

### lastTransitionTime

LastTransitionTime is when the Status last changed.

`lastTransitionTime`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-lasttransitiontime.md "node.json#/$defs/Condition/properties/lastTransitionTime")

#### lastTransitionTime Type

`string`

#### lastTransitionTime Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

### reason

Reason is a CamelCase word summarising why the condition has this status.

`reason`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-reason.md "node.json#/$defs/Condition/properties/reason")

#### reason Type

`string`

### message

Message is a human-readable explanation.

`message`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-condition-properties-message.md "node.json#/$defs/Condition/properties/message")

#### message Type

`string`

## Definitions group NetworkMode

Reference this group by using

```json
{"$ref":"node.json#/$defs/NetworkMode"}
```

| Property | Type | Required | Nullable | Defined by |
| :------- | :--- | :------- | :------- | :--------- |

## Definitions group Node

Reference this group by using

```json
{"$ref":"node.json#/$defs/Node"}
```

| Property                  | Type     | Required | Nullable       | Defined by                                                                                    |
| :------------------------ | :------- | :------- | :------------- | :-------------------------------------------------------------------------------------------- |
| [apiVersion](#apiversion) | `string` | Required | cannot be null | [Node](node-defs-node-properties-apiversion.md "node.json#/$defs/Node/properties/apiVersion") |
| [kind](#kind)             | `string` | Required | cannot be null | [Node](node-defs-node-properties-kind.md "node.json#/$defs/Node/properties/kind")             |
| [metadata](#metadata)     | `object` | Required | cannot be null | [Node](node-defs-node-properties-metadata.md "node.json#/$defs/Node/properties/metadata")     |
| [spec](#spec)             | `object` | Optional | cannot be null | [Node](node-defs-node-properties-spec.md "node.json#/$defs/Node/properties/spec")             |
| [status](#status-1)       | `object` | Optional | cannot be null | [Node](node-defs-node-properties-status.md "node.json#/$defs/Node/properties/status")         |

### apiVersion

APIVersion identifies the versioned schema this resource conforms to, e.g. "caravanserai/v1".

`apiVersion`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-node-properties-apiversion.md "node.json#/$defs/Node/properties/apiVersion")

#### apiVersion Type

`string`

### kind

Kind is the resource type, e.g. "Node", "Project".

`kind`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-node-properties-kind.md "node.json#/$defs/Node/properties/kind")

#### kind Type

`string`

### metadata

ObjectMeta holds identity and classification metadata common to all resources.

`metadata`

* is required

* Type: `object` ([Details](node-defs-node-properties-metadata.md))

* cannot be null

* defined in: [Node](node-defs-node-properties-metadata.md "node.json#/$defs/Node/properties/metadata")

#### metadata Type

`object` ([Details](node-defs-node-properties-metadata.md))

### spec

NodeSpec contains the administrator-declared configuration of a Node.

`spec`

* is optional

* Type: `object` ([Details](node-defs-node-properties-spec.md))

* cannot be null

* defined in: [Node](node-defs-node-properties-spec.md "node.json#/$defs/Node/properties/spec")

#### spec Type

`object` ([Details](node-defs-node-properties-spec.md))

### status

NodeStatus is written by the Agent (heartbeat fields) and the Controller Manager (aggregated state, injected taints).

`status`

* is optional

* Type: `object` ([Details](node-defs-node-properties-status.md))

* cannot be null

* defined in: [Node](node-defs-node-properties-status.md "node.json#/$defs/Node/properties/status")

#### status Type

`object` ([Details](node-defs-node-properties-status.md))

## Definitions group NodeNetworkStatus

Reference this group by using

```json
{"$ref":"node.json#/$defs/NodeNetworkStatus"}
```

| Property                  | Type      | Required | Nullable       | Defined by                                                                                                              |
| :------------------------ | :-------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------------------- |
| [ip](#ip)                 | `string`  | Optional | cannot be null | [Node](node-defs-nodenetworkstatus-properties-ip.md "node.json#/$defs/NodeNetworkStatus/properties/ip")                 |
| [dnsName](#dnsname)       | `string`  | Optional | cannot be null | [Node](node-defs-nodenetworkstatus-properties-dnsname.md "node.json#/$defs/NodeNetworkStatus/properties/dnsName")       |
| [mode](#mode)             | `string`  | Optional | cannot be null | [Node](node-defs-nodenetworkstatus-properties-mode.md "node.json#/$defs/NodeNetworkStatus/properties/mode")             |
| [agentPort](#agentport)   | `integer` | Optional | cannot be null | [Node](node-defs-nodenetworkstatus-properties-agentport.md "node.json#/$defs/NodeNetworkStatus/properties/agentPort")   |
| [throughput](#throughput) | `object`  | Optional | cannot be null | [Node](node-defs-nodenetworkstatus-properties-throughput.md "node.json#/$defs/NodeNetworkStatus/properties/throughput") |

### ip

IP is the Headscale-assigned overlay IP (e.g. "100.64.0.5").

`ip`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus-properties-ip.md "node.json#/$defs/NodeNetworkStatus/properties/ip")

#### ip Type

`string`

### dnsName

DNSName is the MagicDNS FQDN for service discovery.

`dnsName`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus-properties-dnsname.md "node.json#/$defs/NodeNetworkStatus/properties/dnsName")

#### dnsName Type

`string`

### mode

Connectivity mode reported by the Tailscale/Headscale overlay network.

`mode`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus-properties-mode.md "node.json#/$defs/NodeNetworkStatus/properties/mode")

#### mode Type

`string`

#### mode Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value      | Explanation |
| :--------- | :---------- |
| `"Direct"` |             |
| `"DERP"`   |             |

### agentPort

AgentPort is the TCP port the Agent's HTTP server listens on.
Used by caractl to construct the Agent address for port-forward tunnels.

`agentPort`

* is optional

* Type: `integer`

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus-properties-agentport.md "node.json#/$defs/NodeNetworkStatus/properties/agentPort")

#### agentPort Type

`integer`

### throughput

NodeThroughput holds the last measured upload/download speeds of a Node.

`throughput`

* is optional

* Type: `object` ([Details](node-defs-nodenetworkstatus-properties-throughput.md))

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus-properties-throughput.md "node.json#/$defs/NodeNetworkStatus/properties/throughput")

#### throughput Type

`object` ([Details](node-defs-nodenetworkstatus-properties-throughput.md))

## Definitions group NodeSpec

Reference this group by using

```json
{"$ref":"node.json#/$defs/NodeSpec"}
```

| Property                        | Type      | Required | Nullable       | Defined by                                                                                                  |
| :------------------------------ | :-------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------- |
| [hostname](#hostname)           | `string`  | Optional | cannot be null | [Node](node-defs-nodespec-properties-hostname.md "node.json#/$defs/NodeSpec/properties/hostname")           |
| [unschedulable](#unschedulable) | `boolean` | Optional | cannot be null | [Node](node-defs-nodespec-properties-unschedulable.md "node.json#/$defs/NodeSpec/properties/unschedulable") |

### hostname

Hostname is the OS-level hostname of the machine.

`hostname`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodespec-properties-hostname.md "node.json#/$defs/NodeSpec/properties/hostname")

#### hostname Type

`string`

### unschedulable

Unschedulable, when true, prevents new Projects from being scheduled
onto this Node. The TaintsController will automatically add a
NoSchedule Taint when this field is true.

`unschedulable`

* is optional

* Type: `boolean`

* cannot be null

* defined in: [Node](node-defs-nodespec-properties-unschedulable.md "node.json#/$defs/NodeSpec/properties/unschedulable")

#### unschedulable Type

`boolean`

## Definitions group NodeState

Reference this group by using

```json
{"$ref":"node.json#/$defs/NodeState"}
```

| Property | Type | Required | Nullable | Defined by |
| :------- | :--- | :------- | :------- | :--------- |

## Definitions group NodeStatus

Reference this group by using

```json
{"$ref":"node.json#/$defs/NodeStatus"}
```

| Property                        | Type     | Required | Nullable       | Defined by                                                                                                      |
| :------------------------------ | :------- | :------- | :------------- | :-------------------------------------------------------------------------------------------------------------- |
| [state](#state)                 | `string` | Optional | cannot be null | [Node](node-defs-nodestatus-properties-state.md "node.json#/$defs/NodeStatus/properties/state")                 |
| [network](#network)             | `object` | Optional | cannot be null | [Node](node-defs-nodenetworkstatus.md "node.json#/$defs/NodeStatus/properties/network")                         |
| [capacity](#capacity)           | `object` | Optional | cannot be null | [Node](node-defs-nodestatus-properties-capacity.md "node.json#/$defs/NodeStatus/properties/capacity")           |
| [allocatable](#allocatable)     | `object` | Optional | cannot be null | [Node](node-defs-nodestatus-properties-allocatable.md "node.json#/$defs/NodeStatus/properties/allocatable")     |
| [lastHeartbeat](#lastheartbeat) | `string` | Optional | cannot be null | [Node](node-defs-nodestatus-properties-lastheartbeat.md "node.json#/$defs/NodeStatus/properties/lastHeartbeat") |
| [conditions](#conditions)       | `array`  | Optional | cannot be null | [Node](node-defs-nodestatus-properties-conditions.md "node.json#/$defs/NodeStatus/properties/conditions")       |

### state

Top-level health summary computed by the Controller Manager.

`state`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodestatus-properties-state.md "node.json#/$defs/NodeStatus/properties/state")

#### state Type

`string`

#### state Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value        | Explanation |
| :----------- | :---------- |
| `"Ready"`    |             |
| `"NotReady"` |             |
| `"Draining"` |             |

### network

NodeNetworkStatus reports the overlay-network state of a Node.

`network`

* is optional

* Type: `object` ([Details](node-defs-nodenetworkstatus.md))

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus.md "node.json#/$defs/NodeStatus/properties/network")

#### network Type

`object` ([Details](node-defs-nodenetworkstatus.md))

### capacity

ResourceList is a named set of resource quantities — cpu, memory, disk — whose values follow the same string format as Kubernetes: "500m", "4Gi", "100Mbps".

`capacity`

* is optional

* Type: `object` ([Details](node-defs-nodestatus-properties-capacity.md))

* cannot be null

* defined in: [Node](node-defs-nodestatus-properties-capacity.md "node.json#/$defs/NodeStatus/properties/capacity")

#### capacity Type

`object` ([Details](node-defs-nodestatus-properties-capacity.md))

### allocatable

ResourceList is a named set of resource quantities — cpu, memory, disk — whose values follow the same string format as Kubernetes: "500m", "4Gi", "100Mbps".

`allocatable`

* is optional

* Type: `object` ([Details](node-defs-nodestatus-properties-allocatable.md))

* cannot be null

* defined in: [Node](node-defs-nodestatus-properties-allocatable.md "node.json#/$defs/NodeStatus/properties/allocatable")

#### allocatable Type

`object` ([Details](node-defs-nodestatus-properties-allocatable.md))

### lastHeartbeat

LastHeartbeat is the timestamp of the most recent heartbeat received
from the Agent. The NodeController uses this to detect timeouts.

`lastHeartbeat`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodestatus-properties-lastheartbeat.md "node.json#/$defs/NodeStatus/properties/lastHeartbeat")

#### lastHeartbeat Type

`string`

#### lastHeartbeat Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

### conditions

Conditions is a list of observable Node conditions.

`conditions`

* is optional

* Type: `object[]` ([Details](node-defs-condition.md))

* cannot be null

* defined in: [Node](node-defs-nodestatus-properties-conditions.md "node.json#/$defs/NodeStatus/properties/conditions")

#### conditions Type

`object[]` ([Details](node-defs-condition.md))

## Definitions group NodeThroughput

Reference this group by using

```json
{"$ref":"node.json#/$defs/NodeThroughput"}
```

| Property                      | Type     | Required | Nullable       | Defined by                                                                                                            |
| :---------------------------- | :------- | :------- | :------------- | :-------------------------------------------------------------------------------------------------------------------- |
| [download](#download)         | `string` | Optional | cannot be null | [Node](node-defs-nodethroughput-properties-download.md "node.json#/$defs/NodeThroughput/properties/download")         |
| [upload](#upload)             | `string` | Optional | cannot be null | [Node](node-defs-nodethroughput-properties-upload.md "node.json#/$defs/NodeThroughput/properties/upload")             |
| [lastTestTime](#lasttesttime) | `string` | Optional | cannot be null | [Node](node-defs-nodethroughput-properties-lasttesttime.md "node.json#/$defs/NodeThroughput/properties/lastTestTime") |

### download



`download`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodethroughput-properties-download.md "node.json#/$defs/NodeThroughput/properties/download")

#### download Type

`string`

### upload



`upload`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodethroughput-properties-upload.md "node.json#/$defs/NodeThroughput/properties/upload")

#### upload Type

`string`

### lastTestTime



`lastTestTime`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodethroughput-properties-lasttesttime.md "node.json#/$defs/NodeThroughput/properties/lastTestTime")

#### lastTestTime Type

`string`

#### lastTestTime Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

## Definitions group ObjectMeta

Reference this group by using

```json
{"$ref":"node.json#/$defs/ObjectMeta"}
```

| Property                    | Type     | Required | Nullable       | Defined by                                                                                                  |
| :-------------------------- | :------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------- |
| [name](#name)               | `string` | Required | cannot be null | [Node](node-defs-objectmeta-properties-name.md "node.json#/$defs/ObjectMeta/properties/name")               |
| [labels](#labels)           | `object` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-labels.md "node.json#/$defs/ObjectMeta/properties/labels")           |
| [annotations](#annotations) | `object` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-annotations.md "node.json#/$defs/ObjectMeta/properties/annotations") |
| [createdAt](#createdat)     | `string` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-createdat.md "node.json#/$defs/ObjectMeta/properties/createdAt")     |
| [updatedAt](#updatedat)     | `string` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-updatedat.md "node.json#/$defs/ObjectMeta/properties/updatedAt")     |

### name

Name is the unique identifier within its Kind namespace.

`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-name.md "node.json#/$defs/ObjectMeta/properties/name")

#### name Type

`string`

### labels

Labels are arbitrary key/value pairs used for selection and grouping.

`labels`

* is optional

* Type: `object` ([Details](node-defs-objectmeta-properties-labels.md))

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-labels.md "node.json#/$defs/ObjectMeta/properties/labels")

#### labels Type

`object` ([Details](node-defs-objectmeta-properties-labels.md))

### annotations

Annotations are non-identifying metadata (e.g. human-readable hints).

`annotations`

* is optional

* Type: `object` ([Details](node-defs-objectmeta-properties-annotations.md))

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-annotations.md "node.json#/$defs/ObjectMeta/properties/annotations")

#### annotations Type

`object` ([Details](node-defs-objectmeta-properties-annotations.md))

### createdAt

CreatedAt is set by the server on first write.

`createdAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-createdat.md "node.json#/$defs/ObjectMeta/properties/createdAt")

#### createdAt Type

`string`

#### createdAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

### updatedAt

UpdatedAt is set by the server on every write.

`updatedAt`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-updatedat.md "node.json#/$defs/ObjectMeta/properties/updatedAt")

#### updatedAt Type

`string`

#### updatedAt Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

## Definitions group ResourceList

Reference this group by using

```json
{"$ref":"node.json#/$defs/ResourceList"}
```

| Property              | Type     | Required | Nullable       | Defined by                                                                                                  |
| :-------------------- | :------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------- |
| Additional Properties | `string` | Optional | cannot be null | [Node](node-defs-resourcelist-additionalproperties.md "node.json#/$defs/ResourceList/additionalProperties") |

### Additional Properties

Additional properties are allowed, as long as they follow this schema:



* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-resourcelist-additionalproperties.md "node.json#/$defs/ResourceList/additionalProperties")

#### additionalProperties Type

`string`
