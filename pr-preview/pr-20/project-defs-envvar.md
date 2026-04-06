# Untitled object in Project Schema

```txt
project.json#/$defs/ServiceDef/properties/env/items
```

EnvVar is a single environment variable to inject into a container.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## items Type

`object` ([Details](project-defs-envvar.md))

# items Properties

| Property        | Type     | Required | Nullable       | Defined by                                                                                       |
| :-------------- | :------- | :------- | :------------- | :----------------------------------------------------------------------------------------------- |
| [name](#name)   | `string` | Required | cannot be null | [Project](project-defs-envvar-properties-name.md "project.json#/$defs/EnvVar/properties/name")   |
| [value](#value) | `string` | Optional | cannot be null | [Project](project-defs-envvar-properties-value.md "project.json#/$defs/EnvVar/properties/value") |

## name



`name`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-envvar-properties-name.md "project.json#/$defs/EnvVar/properties/name")

### name Type

`string`

## value



`value`

* is optional

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-envvar-properties-value.md "project.json#/$defs/EnvVar/properties/value")

### value Type

`string`
