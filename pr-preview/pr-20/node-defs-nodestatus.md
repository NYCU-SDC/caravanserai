# Untitled object in Node Schema

```txt
node.json#/$defs/NodeStatus
```

NodeStatus is written by the Agent (heartbeat fields) and the Controller Manager (aggregated state, injected taints).

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [node.json\*](../../schemas/node.json "open original schema") |

## NodeStatus Type

`object` ([Details](node-defs-nodestatus.md))

# NodeStatus Properties

| Property                        | Type     | Required | Nullable       | Defined by                                                                                                      |
| :------------------------------ | :------- | :------- | :------------- | :-------------------------------------------------------------------------------------------------------------- |
| [state](#state)                 | `string` | Optional | cannot be null | [Node](node-defs-nodestate.md "node.json#/$defs/NodeStatus/properties/state")                                   |
| [network](#network)             | `object` | Optional | cannot be null | [Node](node-defs-nodenetworkstatus.md "node.json#/$defs/NodeStatus/properties/network")                         |
| [capacity](#capacity)           | `object` | Optional | cannot be null | [Node](node-defs-resourcelist.md "node.json#/$defs/NodeStatus/properties/capacity")                             |
| [allocatable](#allocatable)     | `object` | Optional | cannot be null | [Node](node-defs-resourcelist.md "node.json#/$defs/NodeStatus/properties/allocatable")                          |
| [lastHeartbeat](#lastheartbeat) | `string` | Optional | cannot be null | [Node](node-defs-nodestatus-properties-lastheartbeat.md "node.json#/$defs/NodeStatus/properties/lastHeartbeat") |
| [conditions](#conditions)       | `array`  | Optional | cannot be null | [Node](node-defs-nodestatus-properties-conditions.md "node.json#/$defs/NodeStatus/properties/conditions")       |

## state

Top-level health summary computed by the Controller Manager.

`state`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodestate.md "node.json#/$defs/NodeStatus/properties/state")

### state Type

`string`

### state Constraints

**enum**: the value of this property must be equal to one of the following values:

| Value        | Explanation |
| :----------- | :---------- |
| `"Ready"`    |             |
| `"NotReady"` |             |
| `"Draining"` |             |

## network

NodeNetworkStatus reports the overlay-network state of a Node.

`network`

* is optional

* Type: `object` ([Details](node-defs-nodenetworkstatus.md))

* cannot be null

* defined in: [Node](node-defs-nodenetworkstatus.md "node.json#/$defs/NodeStatus/properties/network")

### network Type

`object` ([Details](node-defs-nodenetworkstatus.md))

## capacity

ResourceList is a named set of resource quantities — cpu, memory, disk — whose values follow the same string format as Kubernetes: "500m", "4Gi", "100Mbps".

`capacity`

* is optional

* Type: `object` ([Details](node-defs-resourcelist.md))

* cannot be null

* defined in: [Node](node-defs-resourcelist.md "node.json#/$defs/NodeStatus/properties/capacity")

### capacity Type

`object` ([Details](node-defs-resourcelist.md))

## allocatable

ResourceList is a named set of resource quantities — cpu, memory, disk — whose values follow the same string format as Kubernetes: "500m", "4Gi", "100Mbps".

`allocatable`

* is optional

* Type: `object` ([Details](node-defs-resourcelist.md))

* cannot be null

* defined in: [Node](node-defs-resourcelist.md "node.json#/$defs/NodeStatus/properties/allocatable")

### allocatable Type

`object` ([Details](node-defs-resourcelist.md))

## lastHeartbeat

LastHeartbeat is the timestamp of the most recent heartbeat received
from the Agent. The NodeController uses this to detect timeouts.

`lastHeartbeat`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodestatus-properties-lastheartbeat.md "node.json#/$defs/NodeStatus/properties/lastHeartbeat")

### lastHeartbeat Type

`string`

### lastHeartbeat Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")

## conditions

Conditions is a list of observable Node conditions.

`conditions`

* is optional

* Type: `object[]` ([Details](node-defs-condition.md))

* cannot be null

* defined in: [Node](node-defs-nodestatus-properties-conditions.md "node.json#/$defs/NodeStatus/properties/conditions")

### conditions Type

`object[]` ([Details](node-defs-condition.md))
