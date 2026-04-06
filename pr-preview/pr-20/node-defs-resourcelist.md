# Untitled object in Node Schema

```txt
node.json#/$defs/ResourceList
```

ResourceList is a named set of resource quantities — cpu, memory, disk — whose values follow the same string format as Kubernetes: "500m", "4Gi", "100Mbps".

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                    |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [node.json\*](../../schemas/node.json "open original schema") |

## ResourceList Type

`object` ([Details](node-defs-resourcelist.md))

# ResourceList Properties

| Property              | Type     | Required | Nullable       | Defined by                                                                                                  |
| :-------------------- | :------- | :------- | :------------- | :---------------------------------------------------------------------------------------------------------- |
| Additional Properties | `string` | Optional | cannot be null | [Node](node-defs-resourcelist-additionalproperties.md "node.json#/$defs/ResourceList/additionalProperties") |

## Additional Properties

Additional properties are allowed, as long as they follow this schema:



* is optional

* Type: `string`

* cannot be null

* defined in: [Node](node-defs-resourcelist-additionalproperties.md "node.json#/$defs/ResourceList/additionalProperties")

### additionalProperties Type

`string`
