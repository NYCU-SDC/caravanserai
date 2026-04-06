# Untitled object in Node Schema

```txt
node.json#/$defs/NodeSpec
```

NodeSpec contains the administrator-declared configuration of a Node.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [node.json\*](../../schemas/node.json "open original schema") |

## NodeSpec Type

`object` ([Details](node-defs-nodespec.md))

# NodeSpec Properties

| Property                        | Type      | Required | Nullable       | Defined by                                                                                                  |
| :------------------------------ | :-------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------- |
| [hostname](#hostname)           | `string`  | Optional | cannot be null | [Node](node-defs-nodespec-properties-hostname.md "node.json#/$defs/NodeSpec/properties/hostname")           |
| [unschedulable](#unschedulable) | `boolean` | Optional | cannot be null | [Node](node-defs-nodespec-properties-unschedulable.md "node.json#/$defs/NodeSpec/properties/unschedulable") |

## hostname

Hostname is the OS-level hostname of the machine.

`hostname`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodespec-properties-hostname.md "node.json#/$defs/NodeSpec/properties/hostname")

### hostname Type

`string`

## unschedulable

Unschedulable, when true, prevents new Projects from being scheduled
onto this Node. The TaintsController will automatically add a
NoSchedule Taint when this field is true.

`unschedulable`

* is optional

* Type: `boolean`

* cannot be null

* defined in: [Node](node-defs-nodespec-properties-unschedulable.md "node.json#/$defs/NodeSpec/properties/unschedulable")

### unschedulable Type

`boolean`
