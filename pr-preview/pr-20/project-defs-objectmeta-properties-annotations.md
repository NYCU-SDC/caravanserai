# Untitled object in Project Schema

```txt
project.json#/$defs/ObjectMeta/properties/annotations
```

Annotations are non-identifying metadata (e.g. human-readable hints).

| Abstract            | Extensible | Status         | Identifiable            | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :---------------------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | Unknown identifiability | Forbidden         | Allowed               | none                | [project.json\*](../../schemas/project.json "open original schema") |

## annotations Type

`object` ([Details](project-defs-objectmeta-properties-annotations.md))

# annotations Properties

| Property              | Type     | Required | Nullable       | Defined by                                                                                                                                                     |
| :-------------------- | :------- | :------- | :------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Additional Properties | `string` | Optional | cannot be null | [Project](project-defs-objectmeta-properties-annotations-additionalproperties.md "project.json#/$defs/ObjectMeta/properties/annotations/additionalProperties") |

## Additional Properties

Additional properties are allowed, as long as they follow this schema:



* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-objectmeta-properties-annotations-additionalproperties.md "project.json#/$defs/ObjectMeta/properties/annotations/additionalProperties")

### additionalProperties Type

`string`
