# Untitled object in Project Schema

```txt
project.json#/$defs/IngressTarget
```

IngressTarget is the backend service and port for an ingress rule.

| Abstract            | Extensible | Status         | Identifiable | Custom Properties | Additional Properties | Access Restrictions | Defined In                                                          |
| :------------------ | :--------- | :------------- | :----------- | :---------------- | :-------------------- | :------------------ | :------------------------------------------------------------------ |
| Can be instantiated | No         | Unknown status | No           | Forbidden         | Forbidden             | none                | [project.json\*](../../schemas/project.json "open original schema") |

## IngressTarget Type

`object` ([Details](project-defs-ingresstarget.md))

# IngressTarget Properties

| Property            | Type      | Required | Nullable       | Defined by                                                                                                         |
| :------------------ | :-------- | :------- | :------------- | :----------------------------------------------------------------------------------------------------------------- |
| [service](#service) | `string`  | Required | cannot be null | [Project](project-defs-ingresstarget-properties-service.md "project.json#/$defs/IngressTarget/properties/service") |
| [port](#port)       | `integer` | Required | cannot be null | [Project](project-defs-ingresstarget-properties-port.md "project.json#/$defs/IngressTarget/properties/port")       |

## service



`service`

* is required

* Type: `string`

* cannot be null

* defined in: [Project](project-defs-ingresstarget-properties-service.md "project.json#/$defs/IngressTarget/properties/service")

### service Type

`string`

## port



`port`

* is required

* Type: `integer`

* cannot be null

* defined in: [Project](project-defs-ingresstarget-properties-port.md "project.json#/$defs/IngressTarget/properties/port")

### port Type

`integer`
