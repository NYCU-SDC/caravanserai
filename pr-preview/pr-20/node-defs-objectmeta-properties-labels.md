# Untitled object in Node Schema

```txt
node.json#/$defs/ObjectMeta/properties/labels
```

Labels are arbitrary key/value pairs used for selection and grouping.

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [node.json\*](../../schemas/node.json "open original schema") |

## labels Type

`object` ([Details](node-defs-objectmeta-properties-labels.md))

# labels Properties

| Property              | Type     | Required | Nullable       | Defined by                                                                                                                                  |
| :-------------------- | :------- | :------- | :------------- | :------------------------------------------------------------------------------------------------------------------------------------------ |
| Additional Properties | `string` | Optional | cannot be null | [Node](node-defs-objectmeta-properties-labels-additionalproperties.md "node.json#/$defs/ObjectMeta/properties/labels/additionalProperties") |

## Additional Properties

Additional properties are allowed, as long as they follow this schema:



* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-objectmeta-properties-labels-additionalproperties.md "node.json#/$defs/ObjectMeta/properties/labels/additionalProperties")

### additionalProperties Type

`string`
