# Untitled object in Node Schema

```txt
node.json#/$defs/NodeThroughput
```

NodeThroughput holds the last measured upload/download speeds of a Node.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [node.json\*](../../schemas/node.json "open original schema") |

## NodeThroughput Type

`object` ([Details](node-defs-nodethroughput.md))

# NodeThroughput Properties

| Property                      | Type     | Required | Nullable       | Defined by                                                                                                            |
| :---------------------------- | :------- | :------- | :------------- | :-------------------------------------------------------------------------------------------------------------------- |
| [download](#download)         | `string` | Optional | cannot be null | [Node](node-defs-nodethroughput-properties-download.md "node.json#/$defs/NodeThroughput/properties/download")         |
| [upload](#upload)             | `string` | Optional | cannot be null | [Node](node-defs-nodethroughput-properties-upload.md "node.json#/$defs/NodeThroughput/properties/upload")             |
| [lastTestTime](#lasttesttime) | `string` | Optional | cannot be null | [Node](node-defs-nodethroughput-properties-lasttesttime.md "node.json#/$defs/NodeThroughput/properties/lastTestTime") |

## download



`download`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodethroughput-properties-download.md "node.json#/$defs/NodeThroughput/properties/download")

### download Type

`string`

## upload



`upload`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodethroughput-properties-upload.md "node.json#/$defs/NodeThroughput/properties/upload")

### upload Type

`string`

## lastTestTime



`lastTestTime`

* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-nodethroughput-properties-lasttesttime.md "node.json#/$defs/NodeThroughput/properties/lastTestTime")

### lastTestTime Type

`string`

### lastTestTime Constraints

**date time**: the string must be a date time string, according to [RFC 3339, section 5.6](https://tools.ietf.org/html/rfc3339 "check the specification")
