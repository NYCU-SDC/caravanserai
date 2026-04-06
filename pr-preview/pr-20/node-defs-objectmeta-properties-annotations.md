# Untitled object in Node Schema

```txt
node.json#/$defs/ObjectMeta/properties/annotations
```

Annotations are non-identifying metadata (e.g. human-readable hints).

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [node.json\*](../../schemas/node.json "open original schema") |

## annotations Type

`object` ([Details](node-defs-objectmeta-properties-annotations.md))

# annotations Properties

| Property              | Type     | Required | Nullable       | Defined by                                                                                                                                            |
| :-------------------- | :------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------------------------------------------------- |
| Additional Properties | `string` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-annotations-additionalproperties.md "node.json#/$defs/ObjectMeta/properties/annotations/additionalProperties") |

## Additional Properties

Additional properties are allowed, as long as they follow this schema:



* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-annotations-additionalproperties.md "node.json#/$defs/ObjectMeta/properties/annotations/additionalProperties")

### additionalProperties Type

`string`
