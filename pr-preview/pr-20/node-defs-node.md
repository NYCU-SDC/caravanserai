# Node Schema

```txt
node.json#/$defs/Node
```

Node represents a physical or virtual machine managed by a Caravanserai Agent.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [node.json\*](../../schemas/node.json "open original schema") |

## Node Type

`object` ([Node](node-defs-node.md))

# Node Properties

| Property                  | Type     | Required | Nullable       | Defined by                                                                                    |
| :------------------------ | :------- | :------- | :------------- | :-------------------------------------------------------------------------------------------- |
| [apiVersion](#apiversion) | `string` | Required | cannot be null | [Node](node-defs-node-properties-apiversion.md "node.json#/$defs/Node/properties/apiVersion") |
| [kind](#kind)             | `string` | Required | cannot be null | [Node](node-defs-node-properties-kind.md "node.json#/$defs/Node/properties/kind")             |
| [metadata](#metadata)     | `object` | Required | cannot be null | [Node](node-defs-objectmeta.md "node.json#/$defs/Node/properties/metadata")                   |
| [spec](#spec)             | `object` | Optional | cannot be null | [Node](node-defs-nodespec.md "node.json#/$defs/Node/properties/spec")                         |
| [status](#status)         | `object` | Optional | cannot be null | [Node](node-defs-nodestatus.md "node.json#/$defs/Node/properties/status")                     |

## apiVersion

APIVersion identifies the versioned schema this resource conforms to, e.g. "caravanserai/v1".

`apiVersion`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-node-properties-apiversion.md "node.json#/$defs/Node/properties/apiVersion")

### apiVersion Type

`string`

## kind

Kind is the resource type, e.g. "Node", "Project".

`kind`

* is required

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-node-properties-kind.md "node.json#/$defs/Node/properties/kind")

### kind Type

`string`

## metadata

ObjectMeta holds identity and classification metadata common to all resources.

`metadata`

* is required

* Type: `object` ([Details](node-defs-objectmeta.md))

* cannot be null

* defined in: [Node](node-defs-objectmeta.md "node.json#/$defs/Node/properties/metadata")

### metadata Type

`object` ([Details](node-defs-objectmeta.md))

## spec

NodeSpec contains the administrator-declared configuration of a Node.

`spec`

* is optional

* Type: `object` ([Details](node-defs-nodespec.md))

* cannot be null

* defined in: [Node](node-defs-nodespec.md "node.json#/$defs/Node/properties/spec")

### spec Type

`object` ([Details](node-defs-nodespec.md))

## status

NodeStatus is written by the Agent (heartbeat fields) and the Controller Manager (aggregated state, injected taints).

`status`

* is optional

* Type: `object` ([Details](node-defs-nodestatus.md))

* cannot be null

* defined in: [Node](node-defs-nodestatus.md "node.json#/$defs/Node/properties/status")

### status Type

`object` ([Details](node-defs-nodestatus.md))
