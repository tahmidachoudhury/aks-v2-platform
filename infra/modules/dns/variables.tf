variable "name" {
  description = "Public DNS zone name, such as azure.example.com."
  type        = string
}

variable "resource_group_name" {
  description = "Resource group for the zone."
  type        = string
}

variable "tags" {
  description = "Tags for the zone."
  type        = map(string)
}
